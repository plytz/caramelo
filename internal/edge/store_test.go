package edge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestARouteTableSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge", "routes.json")
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	table := Table{UpdatedAt: at, Routes: []Route{{
		Host: "FEAT-X.shop.test", Kind: KindHTTPS, App: "shop", Env: "feat-x", Service: "web",
		Drain: 5 * time.Second, CreatedAt: at,
		Targets: []Target{
			{Replica: 1, Port: 20002, State: TargetActive, Inflight: 3},
			{Replica: 2, Port: 20003, State: TargetDraining},
		},
	}}}
	if err := SaveTable(path, table); err != nil {
		t.Fatalf("SaveTable: %v", err)
	}

	got, err := LoadTable(path)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Host != "feat-x.shop.test" {
		t.Fatalf("loaded %+v", got.Routes)
	}
	if got.Routes[0].Drain != 5*time.Second {
		t.Errorf("drain = %s, want the service's own", got.Routes[0].Drain)
	}
	if n := got.Routes[0].Inflight(); n != 0 {
		t.Errorf("in-flight = %d after a reload; counts belong to a running process", n)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != routesFileMode {
		t.Errorf("mode = %v, want %v: the machine's routing is the daemon user's", perm, routesFileMode)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only routes.json", len(entries))
	}
}

func TestAMachineThatNeverExposedAnythingHasNoTable(t *testing.T) {
	got, err := LoadTable(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatalf("LoadTable of a file that is not there = %v, want an empty table", err)
	}
	if len(got.Routes) != 0 {
		t.Errorf("routes = %+v", got.Routes)
	}
}

func TestATableThatCannotBeServedIsRefusedOnLoad(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadTable(broken); err == nil {
		t.Error("LoadTable of a corrupt file = nil, want an error naming it")
	}

	reserved := filepath.Join(dir, "reserved.json")
	if err := os.WriteFile(reserved, []byte(`{"routes":[{"host":"a.test","kind":"tcp"}]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadTable(reserved); err == nil {
		t.Error("LoadTable of a kind M6 does not serve = nil, want an error")
	}
}

func TestSaveTableReplacesWhatWasThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := SaveTable(path, Table{Routes: []Route{route("a.shop.test")}}); err != nil {
		t.Fatalf("SaveTable: %v", err)
	}
	if err := SaveTable(path, Table{Routes: []Route{route("b.shop.test")}}); err != nil {
		t.Fatalf("SaveTable: %v", err)
	}
	got, err := LoadTable(path)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Host != "b.shop.test" {
		t.Errorf("loaded %+v, want only the table that was saved last", got.Routes)
	}
}
