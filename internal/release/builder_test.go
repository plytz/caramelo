package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/state"
)

var goApp = map[string]string{
	"caramelo.yaml":      sampleYAML,
	"go.mod":             "module shop\n\ngo 1.23\n",
	"go.sum":             "",
	"cmd/web/main.go":    "package main\n\nfunc main() {}\n",
	"cmd/worker/main.go": "package main\n\nfunc main() {}\n",
}

func TestBuildToolchainStack(t *testing.T) {
	h := newHarness(t)
	commit := h.commit("first", goApp)
	tree := h.tree(commit)

	var out bytes.Buffer
	res, err := h.b.Build(withIdentity(context.Background(), "commander"),
		BuildRequest{App: "shop", Env: h.branch}, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.Built {
		t.Error("Built is false for a tree nobody had built")
	}
	want := []string{ImageRef("shop", "web", tree), ImageRef("shop", "worker", tree)}
	if !reflect.DeepEqual(res.Images, want) {
		t.Errorf("images = %v, want %v", res.Images, want)
	}

	if res.Release.Tree != tree || res.Release.Commit != commit {
		t.Errorf("release = %s, want tree %s of commit %s", res.Release.Short(), tree, commit)
	}
	if res.Release.ID == 0 {
		t.Error("the release was not recorded")
	}
	if res.Release.BuiltBy != "commander" {
		t.Errorf("BuiltBy = %q, want the caller", res.Release.BuiltBy)
	}

	if res.Release.Config == nil || len(res.Release.Config.Services) != 2 {
		t.Fatalf("the release carries no configuration: %+v", res.Release.Config)
	}
	if got := res.Release.Config.Services[0].Run; got != "go run ./cmd/web" {
		t.Errorf("the stored configuration says run %q", got)
	}

	if n := h.driver.count(); n != 2 {
		t.Fatalf("%d builds, want 2", n)
	}
	spec, ok := h.driver.buildOf(ImageRef("shop", "web", tree))
	if !ok {
		t.Fatal("web was not built")
	}
	if spec.Dockerfile != filepath.Join(".caramelo", "Dockerfile.web") {
		t.Errorf("dockerfile = %q", spec.Dockerfile)
	}
	if spec.NoCache {
		t.Error("a first build asked for --no-cache")
	}
	wantLabels := map[string]string{
		LabelApp: "shop", LabelService: "web", LabelTree: tree, LabelVersion: "0.7.0",
	}
	if !reflect.DeepEqual(spec.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", spec.Labels, wantLabels)
	}

	df := h.driver.dockerfiles[spec.Tag]
	for _, line := range []string{"FROM golang:1.23", "COPY go.mod go.sum ./", "RUN go mod download",
		"COPY . .", `CMD ["sh","-c","go run ./cmd/web"]`} {
		if !strings.Contains(df, line) {
			t.Errorf("the generated Dockerfile has no %q:\n%s", line, df)
		}
	}

	ctxFiles := h.driver.contexts[spec.Tag]
	for _, want := range []string{"caramelo.yaml", "go.mod", filepath.Join("cmd", "web", "main.go")} {
		if !contains(ctxFiles, want) {
			t.Errorf("the build context has no %s: %v", want, ctxFiles)
		}
	}

	if got := h.driver.tags(); !reflect.DeepEqual(got, want) {
		t.Errorf("the machine holds %v, want %v", got, want)
	}

	builds := filepath.Join(h.data, "apps", "shop", "envs", h.branch, "builds")
	if entries, err := os.ReadDir(builds); err == nil && len(entries) != 0 {
		t.Errorf("the export was left behind: %v", names(entries))
	}

	if !strings.Contains(out.String(), ImageRef("shop", "web", tree)) {
		t.Errorf("progress did not name the image:\n%s", out.String())
	}
}

func TestBuildOfAnUnchangedTreeIsANoOp(t *testing.T) {
	h := newHarness(t)
	commit := h.commit("first", goApp)
	ctx := context.Background()
	first, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	before := h.driver.count()

	second, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if second.Built {
		t.Error("Built is true for a tree that was already built")
	}
	if second.Release.ID != first.Release.ID {
		t.Errorf("a second release row was made: %d then %d", first.Release.ID, second.Release.ID)
	}
	if n := h.driver.count(); n != before {
		t.Errorf("%d builds, want the %d of the first call", n, before)
	}
	if len(h.store.releases) != 1 {
		t.Errorf("%d release rows, want 1", len(h.store.releases))
	}

	again := h.commit("nothing changed", nil)
	if again == commit {
		t.Fatal("the fixture did not make a second commit")
	}
	third, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("third Build: %v", err)
	}
	if third.Built || third.Release.ID != first.Release.ID {
		t.Errorf("a commit with the same tree made release %d (built %v), want %d",
			third.Release.ID, third.Built, first.Release.ID)
	}
	if third.Release.Commit != commit {
		t.Errorf("the release's commit moved to %s: a release is the tree, and the row is written once",
			short(third.Release.Commit))
	}
}

func TestBuildRemakesPrunedImages(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	ctx := context.Background()
	first, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := h.driver.RemoveImage(ctx, first.Release.Images["worker"]); err != nil {
		t.Fatal(err)
	}
	before := h.driver.count()

	again, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !again.Built {
		t.Error("Built is false although an image was missing")
	}
	if again.Release.ID != first.Release.ID {
		t.Error("a second release row was made for one tree")
	}

	if n := h.driver.count(); n != before+1 {
		t.Errorf("%d builds, want %d", n, before+1)
	}
	if _, ok := h.driver.buildOf(first.Release.Images["worker"]); !ok {
		t.Error("the missing image was not rebuilt")
	}
}

func TestForceRebuildsAndServiceNarrowsIt(t *testing.T) {
	h := newHarness(t)
	commit := h.commit("first", goApp)
	tree := h.tree(commit)
	ctx := context.Background()
	if _, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	before := h.driver.count()

	res, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch, Force: true, Services: []string{"web"}}, nil)
	if err != nil {
		t.Fatalf("Build --force --service web: %v", err)
	}
	if !res.Built {
		t.Error("Built is false after --force")
	}
	if n := h.driver.count(); n != before+1 {
		t.Errorf("%d builds, want one more than %d: only web was named", n, before)
	}
	spec, ok := h.driver.buildOf(ImageRef("shop", "web", tree))
	if !ok || !spec.NoCache {
		t.Errorf("web was rebuilt with NoCache %v", spec.NoCache)
	}

	if len(res.Release.Images) != 2 {
		t.Errorf("the release has %d images, want 2", len(res.Release.Images))
	}
}

func TestBuildRefusesAnUnknownService(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	_, err := h.b.Build(context.Background(),
		BuildRequest{App: "shop", Env: h.branch, Services: []string{"wbe"}}, nil)
	if err == nil {
		t.Fatal("an unknown service was accepted")
	}
	for _, want := range []string{`"wbe"`, `"web"`, `"worker"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %s", err, want)
		}
	}
}

func TestBuildDockerStackUsesTheReposDockerfile(t *testing.T) {
	h := newHarness(t)
	commit := h.commit("first", map[string]string{
		"Dockerfile":    "FROM alpine:3.20\nCMD [\"/bin/true\"]\n",
		"caramelo.yaml": "name: shop\n",
	})
	tree := h.tree(commit)
	res, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(res.Images) != 1 || res.Images[0] != ImageRef("shop", "web", tree) {
		t.Fatalf("images = %v", res.Images)
	}
	spec, ok := h.driver.buildOf(res.Images[0])
	if !ok {
		t.Fatal("nothing was built")
	}
	if spec.Dockerfile != "" {
		t.Errorf("dockerfile = %q, want the context's own Dockerfile", spec.Dockerfile)
	}
	if files := h.driver.contexts[spec.Tag]; contains(files, ".caramelo") {
		t.Errorf("a Dockerfile was generated for the docker stack: %v", files)
	}
}

func TestBuildExportsTheCommitAndNotTheWorkingCopy(t *testing.T) {
	h := newHarness(t)
	commit := h.commit("first", goApp)
	h.write(map[string]string{"cmd/web/main.go": "package main\n\n// not committed\nfunc main() {}\n"})
	h.write(map[string]string{"secret.txt": "uncommitted"})

	tree := h.tree(commit)
	if _, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	spec, _ := h.driver.buildOf(ImageRef("shop", "web", tree))
	if files := h.driver.contexts[spec.Tag]; contains(files, "secret.txt") {
		t.Errorf("an uncommitted file reached the build context: %v", files)
	}
}

func TestBuildRef(t *testing.T) {
	h := newHarness(t)
	first := h.commit("first", goApp)
	h.git(t, h.src, "tag", "v1.0.0")
	h.git(t, h.src, "push", h.repo, "v1.0.0")
	h.commit("second", map[string]string{"cmd/web/main.go": "package main\n\nfunc main() { _ = 2 }\n"})

	ctx := context.Background()
	res, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch, Ref: "v1.0.0"}, nil)
	if err != nil {
		t.Fatalf("Build --ref v1.0.0: %v", err)
	}
	if res.Release.Commit != first {
		t.Errorf("built %s, want the tag's commit %s", short(res.Release.Commit), short(first))
	}
	if res.Release.Ref != "v1.0.0" {
		t.Errorf("the release's ref is %q", res.Release.Ref)
	}

	head, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if head.Release.Tree == res.Release.Tree {
		t.Error("the tag and the branch head came out as one release")
	}

	if _, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: h.branch, Ref: "v9.9.9"}, nil); err == nil {
		t.Error("an unknown ref was accepted")
	} else if !strings.Contains(err.Error(), "v9.9.9") || !strings.Contains(err.Error(), "push it first") {
		t.Errorf("Build of an unknown ref = %v", err)
	}
}

func TestBuildRefusesACommitWithNothingToRun(t *testing.T) {
	h := newHarness(t)
	h.commit("first", map[string]string{"README.md": "# shop\n"})
	_, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, nil)
	if err == nil {
		t.Fatal("a commit with no app in it was built")
	}
	for _, want := range []string{"nothing to build", "caramelo.yaml", "Dockerfile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %q", err, want)
		}
	}
}

func TestBuildNeedsAnEnvironment(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	ctx := context.Background()
	if _, err := h.b.Build(ctx, BuildRequest{App: "shop", Env: "nope"}, nil); err == nil ||
		!strings.Contains(err.Error(), `no such env "nope"`) {
		t.Errorf("Build of an unknown env = %v", err)
	}
	if _, err := h.b.Build(ctx, BuildRequest{App: "", Env: h.branch}, nil); err == nil {
		t.Error("Build with no app was accepted")
	}
	h.store.apps["other"] = state.App{Name: "other"}
	h.store.envs = append(h.store.envs, state.EnvRecord{
		ID: 2, App: "other", Name: "x", Branch: "x", Worktree: filepath.Join(h.data, "x", "src"),
	})
	if _, err := h.b.Build(ctx, BuildRequest{App: "other", Env: "x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "no repository") {
		t.Errorf("Build of an app with no repository = %v", err)
	}
}

func TestBuilderWithoutItsPieces(t *testing.T) {
	b := New(Config{})
	_, err := b.Build(context.Background(), BuildRequest{App: "shop", Env: "production"}, nil)
	if err == nil {
		t.Fatal("a builder with nothing configured built something")
	}
	for _, want := range []string{"cannot build releases", "state", "container runtime", "git"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %q", err, want)
		}
	}
	if _, err := b.Prune(context.Background(), "shop", 5); err == nil {
		t.Error("a builder with nothing configured pruned something")
	}
}

func TestBuildThatFailsRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	h.driver.buildErr = errors.New("no space left on device")
	_, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, nil)
	if err == nil {
		t.Fatal("a build that could not run reported success")
	}
	if !strings.Contains(err.Error(), "no space left") || !strings.Contains(err.Error(), "caramelo/shop/") {
		t.Errorf("Build = %v, want docker's words and the image it was building", err)
	}
	if len(h.store.releases) != 0 {
		t.Errorf("%d release rows after a failed build", len(h.store.releases))
	}

	builds := filepath.Join(h.data, "apps", "shop", "envs", h.branch, "builds")
	if entries, err := os.ReadDir(builds); err == nil && len(entries) != 0 {
		t.Errorf("the export was left behind: %v", names(entries))
	}
}

func TestBuildEmitsEvents(t *testing.T) {
	h := newHarness(t)
	h.commit("first", goApp)
	var buf bytes.Buffer
	w := progress.New(&buf, progress.FormatJSON)
	if _, err := h.b.Build(context.Background(), BuildRequest{App: "shop", Env: h.branch}, w); err != nil {
		t.Fatalf("Build: %v", err)
	}
	var events []progress.Event
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e progress.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%q is not an event: %v", line, err)
		}
		events = append(events, e)
	}
	if len(events) < 4 {
		t.Fatalf("%d events, want one per service and the two about the release", len(events))
	}
	services := map[string]bool{}
	for _, e := range events {
		if e.Action != ActionBuild {
			t.Errorf("event action %q, want %q", e.Action, ActionBuild)
		}
		if e.App != "shop" || e.Env != h.branch {
			t.Errorf("event %+v does not say which environment", e)
		}
		if e.At.IsZero() {
			t.Errorf("event %+v has no time", e)
		}
		if e.Service != "" {
			services[e.Service] = true
		}
	}
	if !services["web"] || !services["worker"] {
		t.Errorf("the events name %v, want both services", services)
	}
}

func TestPrune(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var ids []int64
	for i := 1; i <= 6; i++ {
		tree := strings.Repeat(string(rune('a'+i-1)), 12)
		row, err := h.store.AddRelease(ctx, state.Release{
			App: "shop", Commit: strings.Repeat("c", 39) + string(rune('0'+i)), Tree: tree,
			ImagesJSON: `{"web":"` + ImageRef("shop", "web", tree) + `"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
		h.driver.images[ImageRef("shop", "web", tree)] = true

		h.store.deploys = append(h.store.deploys, state.Deploy{
			ID: int64(i), EnvID: 1, ReleaseID: row.ID,
			Kind: state.DeployKindDeploy, Status: state.DeployPromoted,
		})
	}

	h.store.envs[0].ReleaseID = ids[5]

	removed, err := h.b.Prune(ctx, "shop", 3)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	want := []string{
		ImageRef("shop", "web", strings.Repeat("a", 12)),
		ImageRef("shop", "web", strings.Repeat("b", 12)),
		ImageRef("shop", "web", strings.Repeat("c", 12)),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}

	if len(h.store.releases) != 6 {
		t.Errorf("%d release rows, want all 6 kept", len(h.store.releases))
	}

	again, err := h.b.Prune(ctx, "shop", 3)
	if err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second prune removed %v", again)
	}
}

func TestPruneKeepsWhatIsLiveAndWhatIsHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var ids []int64
	for i := 1; i <= 4; i++ {
		tree := strings.Repeat(string(rune('a'+i-1)), 12)
		row, err := h.store.AddRelease(ctx, state.Release{
			App: "shop", Tree: tree, ImagesJSON: `{"web":"` + ImageRef("shop", "web", tree) + `"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
		h.driver.images[ImageRef("shop", "web", tree)] = true
	}

	h.store.envs = append(h.store.envs, state.EnvRecord{
		ID: 2, App: "shop", Name: "staging", Branch: "staging",
		Worktree: filepath.Join(h.data, "staging"), ReleaseID: ids[0],
	})
	h.store.deploys = append(h.store.deploys, state.Deploy{
		ID: 1, EnvID: 1, ReleaseID: ids[3], FromReleaseID: ids[1],
		Kind: state.DeployKindDeploy, Status: state.DeployWatching,
	})

	removed, err := h.b.Prune(ctx, "shop", 1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	want := []string{ImageRef("shop", "web", strings.Repeat("c", 12))}
	if !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
}

func TestPruneNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if removed, err := h.b.Prune(ctx, "shop", 5); err != nil || len(removed) != 0 {
		t.Errorf("Prune of an app with no releases = %v, %v", removed, err)
	}
	if _, err := h.b.Prune(ctx, "", 5); err == nil {
		t.Error("Prune with no app was accepted")
	}
}

func TestKeepCount(t *testing.T) {
	if got := keepCount(0); got != 5 {
		t.Errorf("keepCount(0) = %d, want config.DefaultKeep", got)
	}
	if got := keepCount(-1); got != 5 {
		t.Errorf("keepCount(-1) = %d", got)
	}
	if got := keepCount(1000); got != 100 {
		t.Errorf("keepCount(1000) = %d, want config.MaxKeep", got)
	}
	if got := keepCount(7); got != 7 {
		t.Errorf("keepCount(7) = %d", got)
	}
}

func TestFromRecord(t *testing.T) {
	row := &state.Release{
		ID: 3, App: "shop", Commit: strings.Repeat("a", 40), Tree: "abcdef123456", Ref: "HEAD",
		ImagesJSON: `{"web":"caramelo/shop/web:abcdef123456"}`,
		ConfigJSON: `{"name":"shop","services":[{"name":"web","run":"npm start"}]}`,
	}
	rel, err := FromRecord(row)
	if err != nil {
		t.Fatalf("FromRecord: %v", err)
	}
	if ref, ok := rel.Image("web"); !ok || ref != "caramelo/shop/web:abcdef123456" {
		t.Errorf("Image(web) = %q, %v", ref, ok)
	}
	if rel.Config == nil || rel.Config.Name != "shop" {
		t.Errorf("config = %+v", rel.Config)
	}
	if _, err := FromRecord(nil); err == nil {
		t.Error("FromRecord(nil) was accepted")
	}
	for _, bad := range []*state.Release{
		{App: "shop", Tree: "t", ImagesJSON: "{"},
		{App: "shop", Tree: "t", ConfigJSON: "{"},
	} {
		if _, err := FromRecord(bad); err == nil {
			t.Errorf("FromRecord of %+v was accepted", bad)
		} else if !strings.Contains(err.Error(), "shop") {
			t.Errorf("FromRecord = %v, does not name the app", err)
		}
	}
}

func TestImageLabels(t *testing.T) {
	got := ImageLabels("shop", "web", "abcdef1234567890", "0.7.0")
	want := map[string]string{
		LabelApp: "shop", LabelService: "web", LabelTree: "abcdef123456", LabelVersion: "0.7.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("= %v, want %v", got, want)
	}
	if _, ok := got["caramelo.env"]; ok {
		t.Error("a release image is labelled with an environment")
	}
	if got := ImageLabels("shop", "", "", ""); !reflect.DeepEqual(got, map[string]string{LabelApp: "shop"}) {
		t.Errorf("= %v, want only the app", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
