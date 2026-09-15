package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func hubRow() MachineRow {
	return MachineRow{
		Name:      "hub",
		Role:      MachineRoleHub,
		PublicKey: "hubkey",
		Subnet:    "10.86.0.0/16",
		Arch:      "amd64",
		OS:        "linux",
	}
}

func memberRow(name, key, subnet string) MachineRow {
	return MachineRow{
		Name:      name,
		Role:      MachineRoleMember,
		PublicKey: key,
		Subnet:    subnet,
		Arch:      "arm64",
		OS:        "linux",
	}
}

func TestFleetMachinesListTheHubFirst(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	for _, m := range []MachineRow{memberRow("nx3", "k3", "10.88.0.0/16"), memberRow("nx2", "k2", "10.87.0.0/16"), hubRow()} {
		if err := s.PutFleetMachine(ctx, m); err != nil {
			t.Fatalf("PutFleetMachine(%s): %v", m.Name, err)
		}
	}
	got, err := s.FleetMachines(ctx)
	if err != nil {
		t.Fatalf("FleetMachines: %v", err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	want := []string{"hub", "nx2", "nx3"}
	if len(names) != len(want) {
		t.Fatalf("machines = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("machines = %v, want %v", names, want)
		}
	}
	hub, err := s.FleetHub(ctx)
	if err != nil {
		t.Fatalf("FleetHub: %v", err)
	}
	if hub.Name != "hub" {
		t.Errorf("FleetHub = %q, want hub", hub.Name)
	}
}

func TestPutFleetMachineIsAnUpsertByName(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	m := memberRow("nx2", "k2", "10.87.0.0/16")
	if err := s.PutFleetMachine(ctx, m); err != nil {
		t.Fatalf("first put: %v", err)
	}
	m.Private = true
	if err := s.PutFleetMachine(ctx, m); err != nil {
		t.Fatalf("second put: %v", err)
	}
	all, err := s.FleetMachines(ctx)
	if err != nil {
		t.Fatalf("FleetMachines: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("machines = %d, want 1", len(all))
	}
	if !all[0].Private {
		t.Error("the second put did not take: private is false")
	}
}

func TestPutFleetMachineKeepsWhatTheTunnelSaid(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	m := memberRow("nx2", "k2", "10.87.0.0/16")
	if err := s.PutFleetMachine(ctx, m); err != nil {
		t.Fatalf("put: %v", err)
	}
	seen := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	if err := s.SetMachineSeen(ctx, "nx2", "192.168.0.74:51820", seen); err != nil {
		t.Fatalf("SetMachineSeen: %v", err)
	}
	if err := s.SetMachineGauge(ctx, "nx2", `{"memory":{"available_bytes":42}}`); err != nil {
		t.Fatalf("SetMachineGauge: %v", err)
	}

	m.Arch = "arm64"
	if err := s.PutFleetMachine(ctx, m); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, err := s.FleetMachine(ctx, "nx2")
	if err != nil {
		t.Fatalf("FleetMachine: %v", err)
	}
	if got.Endpoint != "192.168.0.74:51820" {
		t.Errorf("endpoint = %q, want the one the handshake recorded", got.Endpoint)
	}
	if !got.LastSeen.Equal(seen) {
		t.Errorf("last_seen = %v, want %v", got.LastSeen, seen)
	}
	if got.GaugeJSON == "" {
		t.Error("the gauge was cleared by a put that carried none")
	}

	later := seen.Add(30 * time.Second)
	if err := s.SetMachineSeen(ctx, "nx2", "", later); err != nil {
		t.Fatalf("SetMachineSeen without an endpoint: %v", err)
	}
	got, err = s.FleetMachine(ctx, "nx2")
	if err != nil {
		t.Fatalf("FleetMachine: %v", err)
	}
	if got.Endpoint != "192.168.0.74:51820" {
		t.Errorf("endpoint = %q after a handshake with none, want it unchanged", got.Endpoint)
	}
	if !got.LastSeen.Equal(later) {
		t.Errorf("last_seen = %v, want %v", got.LastSeen, later)
	}
}

func TestPutFleetMachineRefusesAKeyOrSubnetThatIsSomebodyElses(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	if err := s.PutFleetMachine(ctx, memberRow("nx2", "k2", "10.87.0.0/16")); err != nil {
		t.Fatalf("put nx2: %v", err)
	}
	if err := s.PutFleetMachine(ctx, memberRow("nx3", "k2", "10.88.0.0/16")); !errors.Is(err, ErrExists) {
		t.Errorf("a second name for one key: err = %v, want ErrExists", err)
	}
	if err := s.PutFleetMachine(ctx, memberRow("nx3", "k3", "10.87.0.0/16")); !errors.Is(err, ErrExists) {
		t.Errorf("a second name for one subnet: err = %v, want ErrExists", err)
	}
}

func TestFleetMachineByKeyAndTheMissingCases(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	if err := s.PutFleetMachine(ctx, memberRow("nx2", "k2", "10.87.0.0/16")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.FleetMachineByKey(ctx, "k2")
	if err != nil {
		t.Fatalf("FleetMachineByKey: %v", err)
	}
	if got.Name != "nx2" {
		t.Errorf("name = %q, want nx2", got.Name)
	}
	if _, err := s.FleetMachineByKey(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown key: err = %v, want ErrNotFound", err)
	}
	if _, err := s.FleetMachineByKey(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty key: err = %v, want ErrNotFound", err)
	}
	if _, err := s.FleetMachine(ctx, "nx9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown name: err = %v, want ErrNotFound", err)
	}
	if _, err := s.FleetHub(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("a fleet with no hub row: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteFleetMachine(ctx, "nx2"); err != nil {
		t.Fatalf("DeleteFleetMachine: %v", err)
	}
	if err := s.DeleteFleetMachine(ctx, "nx2"); err != nil {
		t.Fatalf("DeleteFleetMachine again: %v", err)
	}
	if err := s.SetMachineSeen(ctx, "nx2", "x", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("a handshake from a machine that left: err = %v, want ErrNotFound", err)
	}
	if err := s.SetMachineGauge(ctx, "nx2", "{}"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a gauge from a machine that left: err = %v, want ErrNotFound", err)
	}
}

func TestDirectoryUpsertsAndFilters(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	rows := []DirectoryRow{
		{App: "shop", Env: "feat-x", Machine: "nx2", Address: "10.87.1.2", Owner: "agent-a", Mode: EnvModeDev},
		{App: "shop", Env: "feat-y", Machine: "hub", Address: "10.86.1.3", Owner: "agent-b", Mode: EnvModeDev},
		{App: "blog", Env: "production", Machine: "nx2", Address: "10.87.1.4", Owner: "agent-a", Mode: EnvModeRelease, Via: "hub"},
	}
	for _, r := range rows {
		if err := s.PutDirectoryEntry(ctx, r); err != nil {
			t.Fatalf("PutDirectoryEntry(%s/%s): %v", r.App, r.Env, err)
		}
	}
	all, err := s.Directory(ctx, DirectoryFilter{})
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("directory = %d rows, want 3", len(all))
	}

	if all[0].App != "blog" {
		t.Errorf("first row is %s/%s, want blog/production", all[0].App, all[0].Env)
	}
	if all[0].UpdatedAt.IsZero() {
		t.Error("PutDirectoryEntry left updated_at unset")
	}
	for _, tc := range []struct {
		name string
		f    DirectoryFilter
		want int
	}{
		{"by app", DirectoryFilter{App: "shop"}, 2},
		{"by machine", DirectoryFilter{Machine: "nx2"}, 2},
		{"by owner", DirectoryFilter{Owner: "agent-a"}, 2},
		{"by machine and owner", DirectoryFilter{Machine: "nx2", Owner: "agent-b"}, 0},
	} {
		got, err := s.Directory(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.name, len(got), tc.want)
		}
	}

	moved := rows[0]
	moved.Machine = "nx3"
	if err := s.PutDirectoryEntry(ctx, moved); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	one, err := s.DirectoryEntry(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("DirectoryEntry: %v", err)
	}
	if one.Machine != "nx3" {
		t.Errorf("machine = %q, want nx3", one.Machine)
	}
	if err := s.DeleteDirectoryEntry(ctx, "shop", "feat-x"); err != nil {
		t.Fatalf("DeleteDirectoryEntry: %v", err)
	}
	if err := s.DeleteDirectoryEntry(ctx, "shop", "feat-x"); err != nil {
		t.Fatalf("DeleteDirectoryEntry again: %v", err)
	}
	if _, err := s.DirectoryEntry(ctx, "shop", "feat-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a forgotten entry: err = %v, want ErrNotFound", err)
	}
}

func TestReplaceMachineDirectoryIsOneMachinesWholeTruth(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	for _, r := range []DirectoryRow{
		{App: "shop", Env: "old", Machine: "nx2"},
		{App: "shop", Env: "stale", Machine: "nx2"},
		{App: "shop", Env: "elsewhere", Machine: "nx3"},
	} {
		if err := s.PutDirectoryEntry(ctx, r); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	err := s.ReplaceMachineDirectory(ctx, "nx2", []DirectoryRow{
		{App: "shop", Env: "old", Address: "10.87.1.2"},
		{App: "shop", Env: "new", Address: "10.87.1.5"},
	})
	if err != nil {
		t.Fatalf("ReplaceMachineDirectory: %v", err)
	}
	got, err := s.Directory(ctx, DirectoryFilter{Machine: "nx2"})
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if len(got) != 2 || got[0].Env != "new" || got[1].Env != "old" {
		t.Fatalf("nx2 holds %+v, want new and old", got)
	}
	if got[0].Machine != "nx2" {
		t.Errorf("machine = %q, want it filled in from the announcement", got[0].Machine)
	}
	if _, err := s.DirectoryEntry(ctx, "shop", "elsewhere"); err != nil {
		t.Errorf("nx3's row: %v (an announcement touched another machine's rows)", err)
	}

	if err := s.ReplaceMachineDirectory(ctx, "nx2", nil); err != nil {
		t.Fatalf("ReplaceMachineDirectory with nothing: %v", err)
	}
	got, err = s.Directory(ctx, DirectoryFilter{Machine: "nx2"})
	if err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nx2 still holds %d rows after announcing none", len(got))
	}
}

func TestDirectoryRefusesRowsItCannotKey(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	for _, r := range []DirectoryRow{
		{Env: "feat-x", Machine: "nx2"},
		{App: "shop", Machine: "nx2"},
		{App: "shop", Env: "feat-x"},
	} {
		if err := s.PutDirectoryEntry(ctx, r); err == nil {
			t.Errorf("PutDirectoryEntry(%+v) = nil, want an error", r)
		}
	}
	if err := s.ReplaceMachineDirectory(ctx, "", nil); err == nil {
		t.Error("ReplaceMachineDirectory with no machine = nil, want an error")
	}
	err := s.ReplaceMachineDirectory(ctx, "nx2", []DirectoryRow{{App: "shop", Env: "x", Machine: "nx3"}})
	if err == nil {
		t.Error("an announcement carrying another machine's row = nil, want an error")
	}
}

func releaseFixture(t *testing.T, s Store) int64 {
	t.Helper()
	ctx := context.Background()
	if err := s.AddApp(ctx, App{Name: "shop", RepoPath: "/srv/shop.git"}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	r, err := s.AddRelease(ctx, Release{App: "shop", Commit: "c0ffee", Tree: "t1", Machine: "nx2"})
	if err != nil {
		t.Fatalf("AddRelease: %v", err)
	}
	return r.ID
}

func TestReleaseCarriesTheMachineThatBuiltIt(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	id := releaseFixture(t, s)
	got, err := s.Release(ctx, id)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got.Machine != "nx2" {
		t.Errorf("machine = %q, want nx2", got.Machine)
	}
}

func TestReleaseImagesAreOneRowPerArchitectureAndMachine(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	id := releaseFixture(t, s)
	for _, img := range []ReleaseImage{
		{ReleaseID: id, Service: "web", Arch: "arm64", Machine: "nx2", ImageID: "sha256:aa"},
		{ReleaseID: id, Service: "web", Arch: "arm64", Machine: "nx3", ImageID: "sha256:aa"},
		{ReleaseID: id, Service: "web", Arch: "amd64", Machine: "worker2", ImageID: "sha256:bb"},
	} {
		if err := s.AddReleaseImage(ctx, img); err != nil {
			t.Fatalf("AddReleaseImage(%s/%s): %v", img.Arch, img.Machine, err)
		}
	}
	got, err := s.ReleaseImages(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseImages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("images = %d, want 3", len(got))
	}

	if got[0].Arch != "amd64" || got[1].Machine != "nx2" || got[2].Machine != "nx3" {
		t.Fatalf("images out of order: %+v", got)
	}
	if got[0].BuiltAt.IsZero() {
		t.Error("AddReleaseImage left built_at unset")
	}

	again := ReleaseImage{ReleaseID: id, Service: "web", Arch: "arm64", Machine: "nx2", ImageID: "sha256:cc"}
	if err := s.AddReleaseImage(ctx, again); err != nil {
		t.Fatalf("AddReleaseImage twice: %v", err)
	}
	got, err = s.ReleaseImages(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseImages: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("images = %d after a rebuild, want 3", len(got))
	}
	if err := s.DeleteReleaseImage(ctx, id, "web", "arm64", "nx3"); err != nil {
		t.Fatalf("DeleteReleaseImage: %v", err)
	}
	if err := s.DeleteReleaseImage(ctx, id, "web", "arm64", "nx3"); err != nil {
		t.Fatalf("DeleteReleaseImage again: %v", err)
	}
	if err := s.DeleteMachineImages(ctx, "nx2"); err != nil {
		t.Fatalf("DeleteMachineImages: %v", err)
	}
	got, err = s.ReleaseImages(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseImages: %v", err)
	}
	if len(got) != 1 || got[0].Machine != "worker2" {
		t.Fatalf("images after nx2 left: %+v, want worker2's only", got)
	}

	if err := s.AddReleaseImage(ctx, ReleaseImage{ReleaseID: 999, Service: "web", Arch: "amd64", Machine: "nx2"}); err == nil {
		t.Error("an image of a release that does not exist = nil, want an error")
	}

	if err := s.DeleteRelease(ctx, id); err != nil {
		t.Fatalf("DeleteRelease: %v", err)
	}
	got, err = s.ReleaseImages(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseImages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("images = %d after the release was deleted, want 0", len(got))
	}
}

func TestAddReleaseImageRefusesRowsItCannotKey(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	id := releaseFixture(t, s)
	for _, img := range []ReleaseImage{
		{Service: "web", Arch: "amd64", Machine: "nx2"},
		{ReleaseID: id, Arch: "amd64", Machine: "nx2"},
		{ReleaseID: id, Service: "web", Machine: "nx2"},
		{ReleaseID: id, Service: "web", Arch: "amd64"},
	} {
		if err := s.AddReleaseImage(ctx, img); err == nil {
			t.Errorf("AddReleaseImage(%+v) = nil, want an error", img)
		}
	}
}

func TestAJoinTokenIsRedeemedExactlyOnce(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	tok := JoinToken{Hash: "deadbeef", CreatedBy: "commander", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.AddJoinToken(ctx, tok); err != nil {
		t.Fatalf("AddJoinToken: %v", err)
	}
	if err := s.AddJoinToken(ctx, tok); !errors.Is(err, ErrExists) {
		t.Errorf("the same hash twice: err = %v, want ErrExists", err)
	}
	got, err := s.JoinToken(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("JoinToken: %v", err)
	}
	if got.Used() {
		t.Error("a fresh token reads as used")
	}
	if !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, tok.ExpiresAt)
	}
	used := now.Add(time.Minute)
	if err := s.RedeemJoinToken(ctx, "deadbeef", "nx2", used); err != nil {
		t.Fatalf("RedeemJoinToken: %v", err)
	}
	if err := s.RedeemJoinToken(ctx, "deadbeef", "nx3", used); !errors.Is(err, ErrExists) {
		t.Errorf("a second redemption: err = %v, want ErrExists", err)
	}
	got, err = s.JoinToken(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("JoinToken: %v", err)
	}

	if got.UsedBy != "nx2" || !got.UsedAt.Equal(used) {
		t.Errorf("used_by/used_at = %q/%v, want nx2/%v", got.UsedBy, got.UsedAt, used)
	}
	if !got.Used() {
		t.Error("a redeemed token does not read as used")
	}
	if err := s.RedeemJoinToken(ctx, "nosuchhash", "nx2", used); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeeming an unknown token: err = %v, want ErrNotFound", err)
	}
	if _, err := s.JoinToken(ctx, "nosuchhash"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading an unknown token: err = %v, want ErrNotFound", err)
	}
	if err := s.RedeemJoinToken(ctx, "deadbeef", "", used); err == nil {
		t.Error("redeeming for no machine = nil, want an error")
	}
}

func TestExpiredJoinTokensAreForgottenButUsedOnesAreNot(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, tok := range []JoinToken{
		{Hash: "expired", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		{Hash: "live", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Hash: "spent", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
			UsedBy: "nx2", UsedAt: now.Add(-90 * time.Minute)},
	} {
		if err := s.AddJoinToken(ctx, tok); err != nil {
			t.Fatalf("AddJoinToken(%s): %v", tok.Hash, err)
		}
	}
	n, err := s.DeleteExpiredJoinTokens(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredJoinTokens: %v", err)
	}
	if n != 1 {
		t.Errorf("forgot %d tokens, want 1", n)
	}
	if _, err := s.JoinToken(ctx, "expired"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the expired token: err = %v, want ErrNotFound", err)
	}
	if _, err := s.JoinToken(ctx, "live"); err != nil {
		t.Errorf("the live token: %v", err)
	}
	if _, err := s.JoinToken(ctx, "spent"); err != nil {
		t.Errorf("the spent token was forgotten: %v (its message is what it is for)", err)
	}
}

func TestAnEnvironmentsOwnerStartsAsItsCreatorAndMovesWithAHandoff(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	if err := s.AddApp(ctx, App{Name: "shop", RepoPath: "/srv/shop.git"}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	rec, err := s.CreateEnv(ctx, EnvRecord{App: "shop", Name: "feat-x", PortBase: 20000, PortCount: 32, CreatedBy: "agent-a"})
	if err != nil {
		t.Fatalf("CreateEnv: %v", err)
	}
	if rec.Owner != "agent-a" {
		t.Errorf("owner = %q, want the creator", rec.Owner)
	}
	if err := s.SetEnvOwner(ctx, rec.ID, "agent-b"); err != nil {
		t.Fatalf("SetEnvOwner: %v", err)
	}
	if err := s.SetEnvVia(ctx, rec.ID, "hub"); err != nil {
		t.Fatalf("SetEnvVia: %v", err)
	}
	got, err := s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	if got.Owner != "agent-b" || got.Via != "hub" {
		t.Fatalf("owner/via = %q/%q, want agent-b/hub", got.Owner, got.Via)
	}
	if got.CreatedBy != "agent-a" {
		t.Errorf("created_by = %q, want agent-a: a handoff is not a rewrite of history", got.CreatedBy)
	}

	stale := *rec
	stale.Owner, stale.Via = "agent-a", ""
	stale.Status = EnvReady
	if err := s.UpdateEnv(ctx, stale); err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}
	got, err = s.Env(ctx, "shop", "feat-x")
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	if got.Owner != "agent-b" || got.Via != "hub" {
		t.Errorf("owner/via = %q/%q after a stale UpdateEnv, want agent-b/hub", got.Owner, got.Via)
	}
	if err := s.SetEnvOwner(ctx, 9999, "agent-c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvOwner on a missing env: err = %v, want ErrNotFound", err)
	}
	if err := s.SetEnvVia(ctx, 9999, "hub"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvVia on a missing env: err = %v, want ErrNotFound", err)
	}
}

func TestAnEventRemembersWhichMachineItHappenedOn(t *testing.T) {
	s, _ := tempDB(t)
	ctx := context.Background()
	if err := s.AddApp(ctx, App{Name: "shop", RepoPath: "/srv/shop.git"}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	rec, err := s.CreateEnv(ctx, EnvRecord{App: "shop", Name: "feat-x", PortBase: 20000, PortCount: 32})
	if err != nil {
		t.Fatalf("CreateEnv: %v", err)
	}
	if err := s.AddEvent(ctx, EnvEvent{EnvID: rec.ID, Action: "up", Status: "ok", Machine: "nx2"}); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}

	if err := s.AddEvent(ctx, EnvEvent{EnvID: rec.ID, Action: "down", Status: "ok"}); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}
	byEnv, err := s.Events(ctx, rec.ID, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(byEnv) != 2 || byEnv[0].Machine != "" || byEnv[1].Machine != "nx2" {
		t.Fatalf("events = %+v, want the newest with no machine and the older on nx2", byEnv)
	}
	feed, err := s.QueryEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(feed) != 2 || feed[1].Machine != "nx2" {
		t.Fatalf("feed = %+v, want the older event on nx2", feed)
	}

	if got := feed[1].Progress().Machine; got != "nx2" {
		t.Errorf("Progress().Machine = %q, want nx2", got)
	}
	back := EventFromProgress(rec.ID, feed[1].Progress())
	if back.Machine != "nx2" {
		t.Errorf("EventFromProgress lost the machine: %q", back.Machine)
	}
}
