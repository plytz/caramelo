package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func runFleet(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestFleetAddListDefaultRemove(t *testing.T) {
	isolateOnACommander(t)

	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add: exit %d: %s", code, stderr)
	}
	if code, _, stderr := runFleet(t, "fleet", "add", "work", "ops@hub.work.example:4023"); code != ExitOK {
		t.Fatalf("fleet add work: exit %d: %s", code, stderr)
	}

	cfg, err := remote.LoadCommanderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commander.DefaultFleet != "home" {
		t.Errorf("default_fleet = %q, want the first fleet added", cfg.Commander.DefaultFleet)
	}
	if got := cfg.Commander.Fleets["home"].Hub; got != "eric@box.example.com:4022" {
		t.Errorf("home hub = %q, want the ssh port filled in", got)
	}

	code, stdout, stderr := runFleet(t, "fleet", "list")
	if code != ExitOK {
		t.Fatalf("fleet list: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"home", "work", "hub.work.example", "yes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("fleet list = %q, want it to show %q", stdout, want)
		}
	}

	if code, _, stderr := runFleet(t, "fleet", "default", "work"); code != ExitOK {
		t.Fatalf("fleet default: exit %d: %s", code, stderr)
	}
	cfg, _ = remote.LoadCommanderConfig()
	if cfg.Commander.DefaultFleet != "work" {
		t.Errorf("default_fleet = %q, want work", cfg.Commander.DefaultFleet)
	}

	if code, _, stderr := runFleet(t, "fleet", "remove", "work"); code != ExitOK {
		t.Fatalf("fleet remove: exit %d: %s", code, stderr)
	}
	cfg, _ = remote.LoadCommanderConfig()
	if _, ok := cfg.Commander.Fleets["work"]; ok {
		t.Error("fleet remove left the fleet in the config")
	}
	if cfg.Commander.DefaultFleet != "" {
		t.Errorf("default_fleet = %q, want it cleared with the fleet it named", cfg.Commander.DefaultFleet)
	}
}

func TestFleetAddKeepsThePinnedKeyWhenTheHubMoves(t *testing.T) {
	isolateOnACommander(t)
	const key = "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="

	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add: exit %d: %s", code, stderr)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if _, err := cfg.Pin("home", key); err != nil {
		t.Fatal(err)
	}
	if err := remote.SaveCommanderConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@moved.example.com"); code != ExitOK {
		t.Fatalf("re-adding a fleet: exit %d: %s", code, stderr)
	}
	cfg, _ = remote.LoadCommanderConfig()
	f := cfg.Commander.Fleets["home"]
	if f.Hub != "eric@moved.example.com:4022" {
		t.Errorf("hub = %q, want the new address", f.Hub)
	}
	if f.PublicKey != key {
		t.Errorf("public_key = %q, want the pinned key to survive a move", f.PublicKey)
	}
}

func TestFleetRefusesNamesAndFleetsItDoesNotKnow(t *testing.T) {
	isolateOnACommander(t)

	if code, _, stderr := runFleet(t, "fleet", "add", "Home Fleet", "eric@box"); code != ExitUsage {
		t.Errorf("fleet add of a name that is no slug: exit %d, want %d: %s", code, ExitUsage, stderr)
	}
	if code, _, stderr := runFleet(t, "fleet", "default", "nowhere"); code != ExitUsage {
		t.Errorf("fleet default of an unknown fleet: exit %d, want %d: %s", code, ExitUsage, stderr)
	}
	code, _, stderr := runFleet(t, "fleet", "remove", "nowhere")
	if code != ExitUsage {
		t.Errorf("fleet remove of an unknown fleet: exit %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "none") {
		t.Errorf("stderr = %q, want it to say which fleets there are", stderr)
	}
}

func TestFleetListJSONIsEmptyNotNull(t *testing.T) {
	isolateOnACommander(t)
	code, stdout, stderr := runFleet(t, "fleet", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var fs []fleetView
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &fs); err != nil {
		t.Fatalf("fleet list --json is not JSON: %v (%q)", err, stdout)
	}
	if len(fs) != 0 {
		t.Errorf("fleets = %+v, want none", fs)
	}
}

func fakeAppList(t *testing.T, names ...string) *[]string {
	t.Helper()
	asked := &[]string{}
	old := forward
	forward = func(_ context.Context, a *app) (int, error) {
		*asked = append(*asked, a.fleet)
		body, err := json.Marshal(appNames(names))
		if err != nil {
			return ExitError, err
		}
		_, _ = a.stdout.Write(body)
		return ExitOK, nil
	}
	t.Cleanup(func() { forward = old })
	return asked
}

func appNames(names []string) []struct {
	Name string `json:"name"`
} {
	out := make([]struct {
		Name string `json:"name"`
	}, 0, len(names))
	for _, n := range names {
		out = append(out, struct {
			Name string `json:"name"`
		}{n})
	}
	return out
}

func TestAnAppIsRecordedOnTheFleetThatHoldsIt(t *testing.T) {
	isolateOnACommander(t)
	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add: exit %d: %s", code, stderr)
	}
	asked := fakeAppList(t, "shop", "blog")

	if err := rememberAppFleet(context.Background(), "home", "shop"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if cfg.FleetForApp("shop") != "home" {
		t.Errorf("fleets = %+v, want shop recorded on home", cfg.Commander.Fleets)
	}
	if len(*asked) != 1 || (*asked)[0] != "home" {
		t.Errorf("app list was asked of %q, want the fleet once", *asked)
	}

	if err := rememberAppFleet(context.Background(), "home", "shop"); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 {
		t.Errorf("app list was asked %d times, want once: an app already recorded costs no round trip", len(*asked))
	}
}

func TestAnAppTheHubDoesNotHoldIsNotRecorded(t *testing.T) {
	isolateOnACommander(t)
	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add: exit %d: %s", code, stderr)
	}
	fakeAppList(t, "blog")

	if err := rememberAppFleet(context.Background(), "home", "shop"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if got := cfg.Commander.Fleets["home"].Apps; len(got) != 0 {
		t.Errorf("apps = %q, want none: the hub was asked and does not hold shop", got)
	}
}

func TestFleetRemoveForgetsTheTunnelItKept(t *testing.T) {
	isolateOnACommander(t)
	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add: exit %d: %s", code, stderr)
	}
	if err := (pinnedRecords{}).Save(homeRecord()); err != nil {
		t.Fatal(err)
	}
	keys := &vpnclient.FileKeyStore{}
	if _, _, err := keys.Ensure("home"); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runFleet(t, "fleet", "remove", "home"); code != ExitOK {
		t.Fatalf("fleet remove: exit %d: %s", code, stderr)
	}
	if _, err := (&vpnclient.FileRecordStore{}).Load("home"); !errors.Is(err, vpnclient.ErrNoKey) {
		t.Errorf("the tunnel record survived the removal (%v)", err)
	}
	if _, err := keys.Load("home"); !errors.Is(err, vpnclient.ErrNoKey) {
		t.Errorf("the key survived the removal (%v)", err)
	}

	if code, _, stderr := runFleet(t, "fleet", "add", "home", "eric@box.example.com"); code != ExitOK {
		t.Fatalf("fleet add again: exit %d: %s", code, stderr)
	}
	rebuilt := homeRecord()
	rebuilt.MachineKey = anotherKey
	if err := (pinnedRecords{}).Save(rebuilt); err != nil {
		t.Fatalf("a hub rebuilt at the same address must be adopted after 'fleet remove': %v", err)
	}
}

const oneFleetFile = `name: laptop
role: commander
commander:
  default_fleet: home
  fleets:
    home:
      hub: caramelo@box.example:4022
`

func TestACommandRecordsItsAppOnTheFleetItReached(t *testing.T) {
	dir := isolate(t)
	t.Setenv("CARAMELO_APP", "")
	writeCommanderFile(t, dir, oneFleetFile)
	fakeGit(t, nil)
	f := &fakeTransport{apps: []string{"shop", "blog"}}
	f.install(t)

	if code, _, stderr := runFleet(t, "env", "list", "--app", "shop"); code != ExitOK {
		t.Fatalf("env list: exit %d: %s", code, stderr)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if cfg.FleetForApp("shop") != "home" {
		t.Errorf("fleets = %+v, want shop recorded on the fleet the command reached", cfg.Commander.Fleets)
	}
	if len(f.seen) != 2 {
		t.Errorf("transports = %+v, want the command itself and the app list that confirms it", f.seen)
	}
}

func TestAFailedCommandRecordsNoApp(t *testing.T) {
	dir := isolate(t)
	t.Setenv("CARAMELO_APP", "")
	writeCommanderFile(t, dir, oneFleetFile)
	fakeGit(t, nil)
	f := &fakeTransport{apps: []string{"shop"}, code: ExitError}
	f.install(t)

	if code, _, _ := runFleet(t, "env", "list", "--app", "shop"); code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if got := cfg.Commander.Fleets["home"].Apps; len(got) != 0 {
		t.Errorf("apps = %q, want none: the command failed", got)
	}
	if len(f.seen) != 1 {
		t.Errorf("transports = %+v, want no app list after a failure", f.seen)
	}
}

func TestAnAppThatCannotBeRecordedIsOnlyAWarning(t *testing.T) {
	dir := isolate(t)
	t.Setenv("CARAMELO_APP", "")
	writeCommanderFile(t, dir, oneFleetFile)
	fakeGit(t, nil)
	f := &fakeTransport{listErr: errors.New("the hub did not answer")}
	f.install(t)

	code, _, stderr := runFleet(t, "env", "list", "--app", "shop")
	if code != ExitOK {
		t.Fatalf("exit %d, want the command's own code: %s", code, stderr)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "shop") {
		t.Errorf("stderr = %q, want a warning naming the app", stderr)
	}
	cfg, _ := remote.LoadCommanderConfig()
	if got := cfg.Commander.Fleets["home"].Apps; len(got) != 0 {
		t.Errorf("apps = %q, want none", got)
	}
}
