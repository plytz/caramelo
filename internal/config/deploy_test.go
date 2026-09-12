package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseV3(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v3.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	web, ok := app.Service("web")
	if !ok {
		t.Fatal("no web service")
	}
	if web.Resources == nil {
		t.Fatal("web has no resources")
	}
	if got, want := web.Resources.Memory, int64(512<<20); got != want {
		t.Errorf("web memory = %d, want %d", got, want)
	}
	if got, want := web.Resources.CPU, 1.0; got != want {
		t.Errorf("web cpu = %v, want %v", got, want)
	}
	d := app.Deploy
	if d == nil {
		t.Fatal("no deploy block")
	}
	if d.Before != "npm run migrate" || d.Check != "npm run smoke" {
		t.Errorf("hooks = %q / %q", d.Before, d.Check)
	}
	if d.Watch != time.Minute {
		t.Errorf("watch = %s, want 1m", d.Watch)
	}
	if d.MaxErrors != "5%" {
		t.Errorf("max_errors = %q, want %q (kept as written)", d.MaxErrors, "5%")
	}
	if got := d.MaxErrorRate(); got != 0.05 {
		t.Errorf("MaxErrorRate = %v, want 0.05", got)
	}
	if d.PromotePolicy() != PromoteAuto || d.KeepReleases() != 5 {
		t.Errorf("promote/keep = %s/%d", d.PromotePolicy(), d.KeepReleases())
	}
	prod, ok := app.Override("production")
	if !ok {
		t.Fatal("no production override")
	}
	if got := prod.Hosts; len(got) != 2 || got[0] != "shop.example.com" || got[1] != "www.shop.example.com" {
		t.Errorf("production hosts = %v", got)
	}
	if got := prod.Services["web"].Replicas; got != 3 {
		t.Errorf("production web replicas = %d, want 3", got)
	}
}

func TestDeployDefaults(t *testing.T) {
	var d *Deploy
	if got := d.WatchWindow(); got != DefaultWatch {
		t.Errorf("WatchWindow = %s, want %s", got, DefaultWatch)
	}
	if got := d.MaxErrorRate(); got != 0.05 {
		t.Errorf("MaxErrorRate = %v, want 0.05", got)
	}
	if got := d.PromotePolicy(); got != PromoteAuto {
		t.Errorf("PromotePolicy = %s, want %s", got, PromoteAuto)
	}
	if got := d.KeepReleases(); got != DefaultKeep {
		t.Errorf("KeepReleases = %d, want %d", got, DefaultKeep)
	}

	empty := &Deploy{}
	if empty.WatchWindow() != DefaultWatch || empty.KeepReleases() != DefaultKeep {
		t.Errorf("an empty deploy block is not the defaults: %+v", empty)
	}
}

func TestForEnv(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v3.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prod := app.ForEnv("production")
	web, _ := prod.Service("web")
	if got := web.Replicas(); got != 3 {
		t.Errorf("production web replicas = %d, want 3", got)
	}
	if got, want := web.Resources.Memory, int64(1<<30); got != want {
		t.Errorf("production web memory = %d, want %d", got, want)
	}

	if got, want := web.Resources.CPU, 1.0; got != want {
		t.Errorf("production web cpu = %v, want %v (merged field by field)", got, want)
	}
	if got := prod.Deploy.WatchWindow(); got != 5*time.Minute {
		t.Errorf("production watch = %s, want 5m", got)
	}
	if got := prod.Deploy.PromotePolicy(); got != PromoteManual {
		t.Errorf("production promote = %s, want manual", got)
	}

	if prod.Deploy.Before != "npm run migrate" {
		t.Errorf("production before = %q, want the app's", prod.Deploy.Before)
	}

	other := app.ForEnv("feat-x")
	owe, _ := other.Service("web")
	if got := owe.Replicas(); got != 2 {
		t.Errorf("feat-x web replicas = %d, want the app's 2", got)
	}
	if got := other.Deploy.WatchWindow(); got != time.Minute {
		t.Errorf("feat-x watch = %s, want the app's 1m", got)
	}

	base, _ := app.Service("web")
	if got := base.Replicas(); got != 2 {
		t.Errorf("the app's own web replicas became %d: ForEnv must copy", got)
	}
	if got, want := base.Resources.Memory, int64(512<<20); got != want {
		t.Errorf("the app's own web memory became %d: ForEnv must copy", got)
	}
	if got := app.Deploy.WatchWindow(); got != time.Minute {
		t.Errorf("the app's own watch became %s: ForEnv must copy", got)
	}
}

func TestHostsFor(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v3.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := app.HostsFor("production", "web")
	if len(got) != 2 || got[0] != "shop.example.com" || got[1] != "www.shop.example.com" {
		t.Errorf("HostsFor(production, web) = %v", got)
	}
	if got := app.HostsFor("staging", "web"); len(got) != 1 || got[0] != "staging.shop.example.com" {
		t.Errorf("HostsFor(staging, web) = %v", got)
	}

	if got := app.HostsFor("feat-x", "web"); len(got) != 1 || got[0] != "feat-x.shop.example.com" {
		t.Errorf("HostsFor(feat-x, web) = %v, want the derived name", got)
	}

	if got := app.HostsFor("production", "worker"); got != nil {
		t.Errorf("HostsFor(production, worker) = %v, want none", got)
	}

	hosts := app.HostsFor("production", "web")
	hosts[0] = "evil.example.com"
	if again := app.HostsFor("production", "web"); again[0] != "shop.example.com" {
		t.Errorf("HostsFor returned the stored slice: %v", again)
	}
}

func TestV3Errors(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		says string
	}{
		{"reserved backups", "name: a\nbackups:\n  db: daily\n", "not supported yet"},
		{"reserved alerts", "name: a\nalerts:\n  webhook: x\n", "not supported yet"},
		{"deploy unknown key", "name: a\ndeploy:\n  canary: 10%\n", `unknown key "canary"`},
		{"watch is a duration", "name: a\ndeploy:\n  watch: 60\n", "must be a duration"},
		{"max_errors is a percentage", "name: a\ndeploy:\n  max_errors: 5\n", "must be a percentage"},
		{"max_errors out of range", "name: a\ndeploy:\n  max_errors: 300%\n", "out of range"},
		{"promote is auto or manual", "name: a\ndeploy:\n  promote: canary\n", "unknown promote"},
		{"keep out of range", "name: a\ndeploy:\n  keep: 0\n", "out of range"},
		{"envs unknown key", "name: a\nenvs:\n  production:\n    run: npm start\n", "may only override"},
		{"envs service unknown key",
			"name: a\nservices:\n  web:\n    run: x\nenvs:\n  production:\n    services:\n      web:\n        run: y\n",
			"may only override"},
		{"envs service must exist",
			"name: a\nservices:\n  web:\n    run: x\nenvs:\n  production:\n    services:\n      api: {replicas: 2}\n",
			`there is no service "api"`},
		{"envs hosts is a hostname", "name: a\nenvs:\n  production:\n    hosts: [http://shop.example.com]\n", "not a URL"},
		{"envs hosts duplicate", "name: a\nenvs:\n  p:\n    hosts: [a.example.com, a.example.com]\n", "duplicate hostname"},
		{"resources unknown key",
			"name: a\nservices:\n  web:\n    run: x\n    resources: {swap: 1g}\n", `unknown key "swap"`},
		{"memory unit", "name: a\nservices:\n  web:\n    run: x\n    resources: {memory: 512q}\n", "unknown size unit"},
		{"memory below docker's minimum",
			"name: a\nservices:\n  web:\n    run: x\n    resources: {memory: 1m}\n", "below the 6m"},
		{"cpu is a number", "name: a\nservices:\n  web:\n    run: x\n    resources: {cpu: lots}\n", "number of CPUs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("parsed %q, want an error about %q", tc.yaml, tc.says)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %v, want it to mention %q", err, tc.says)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"512m", 512 << 20},
		{"1g", 1 << 30},
		{"2gb", 2 << 30},
		{"1024k", 1 << 20},
		{"1.5g", 3 << 29},
		{"67108864", 64 << 20},
		{" 128M ", 128 << 20},
	} {
		got, err := ParseMemory(tc.in)
		if err != nil {
			t.Errorf("ParseMemory(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemory(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "m", "-1g", "0", "1t", "1 gig"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) = no error", bad)
		}
	}
	for _, tc := range []struct {
		in   int64
		want string
	}{{512 << 20, "512m"}, {1 << 30, "1g"}, {1 << 10, "1k"}, {1500, "1500"}, {0, ""}} {
		if got := FormatMemory(tc.in); got != tc.want {
			t.Errorf("FormatMemory(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResourceArgs(t *testing.T) {
	r := Resources{Memory: 512 << 20, CPU: 1.5}
	if got, want := r.MemoryArg(), "536870912"; got != want {
		t.Errorf("MemoryArg = %q, want %q", got, want)
	}
	if got, want := r.CPUArg(), "1.5"; got != want {
		t.Errorf("CPUArg = %q, want %q", got, want)
	}
	if got, want := r.String(), "512m, cpu 1.5"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	var none Resources
	if !none.Empty() || none.MemoryArg() != "" || none.CPUArg() != "" {
		t.Errorf("the zero Resources is not unlimited: %+v", none)
	}
}

func TestSecretReferences(t *testing.T) {
	app, err := Load(filepath.Join("testdata", "v3.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	views := Views{
		Network: ExpandContext{
			App: "shop", Env: "production", Port: 3000,
			Deps:     map[string]DepAddr{"db": {Host: "db", Port: 5432}},
			Services: map[string]DepAddr{"web": {Host: "web", Port: 3000}},
			View:     ViewNetwork,
		},
	}
	secrets := Secrets{
		Lookup: func(name string) (string, bool) {
			v, ok := map[string]string{"STRIPE_KEY": "sk_live_x"}[name]
			return v, ok
		},
		DepPassword: func(dep string) (string, bool) {
			v, ok := map[string]string{"db": "chosen"}[dep]
			return v, ok
		},
	}
	res, err := app.ResolveWith(ViewNetwork, views, secrets)
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	web, _ := res.Service("web")
	if got, want := web.Env["STRIPE_KEY"], "sk_live_x"; got != want {
		t.Errorf("STRIPE_KEY = %q, want %q", got, want)
	}
	if got, want := web.Env["DATABASE_URL"], "postgres://postgres:chosen@db:5432/postgres"; got != want {
		t.Errorf("DATABASE_URL = %q, want %q", got, want)
	}

	plain, err := app.Resolve(ViewNetwork, views)
	if err != nil {
		t.Fatalf("Resolve with no vault: %v", err)
	}
	pweb, _ := plain.Service("web")
	if got := pweb.Env["STRIPE_KEY"]; got != "" {
		t.Errorf("STRIPE_KEY with no vault = %q, want empty", got)
	}

	empty := Secrets{
		Lookup:      func(string) (string, bool) { return "", false },
		DepPassword: func(string) (string, bool) { return "", false },
	}
	_, err = app.ResolveWith(ViewNetwork, views, empty)
	if err == nil {
		t.Fatal("ResolveWith an empty vault: no error")
	}
	if !strings.Contains(err.Error(), "secrets set") {
		t.Errorf("error = %v, want it to say how to set the secret", err)
	}
}

func TestUnknownReferencesStillFail(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"no such dep", "${deps.nope.password}"},
		{"no such field", "${deps.db.secret}"},
		{"not a variable name", "${secrets.not-a-name}"},
	} {
		yaml := "name: a\ndeps:\n  db: postgres:16\nenv:\n  X: " + tc.value + "\n"
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: parsed %q, want an unknown reference", tc.name, tc.value)
		}
	}
}

func TestDepPasswordSecret(t *testing.T) {
	for in, want := range map[string]string{
		"db": "DB_PASSWORD", "cache": "CACHE_PASSWORD", "main-db": "MAIN_DB_PASSWORD",
	} {
		if got := DepPasswordSecret(in); got != want {
			t.Errorf("DepPasswordSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaxErrorsNoneAndZero(t *testing.T) {
	for _, tc := range []struct {
		written string
		want    float64
	}{
		{"", 0.05},
		{"5%", 0.05},
		{"0%", 0},
		{"none", NoMaxErrors},
		{"None", NoMaxErrors},
		{"nonsense", 0.05},
	} {
		d := &Deploy{MaxErrors: tc.written}
		if got := d.MaxErrorRate(); got != tc.want {
			t.Errorf("max_errors %q = %v, want %v", tc.written, got, tc.want)
		}
	}
	if _, err := ParseMaxErrors("0%"); err != nil {
		t.Errorf("0%% is a legal policy: %v", err)
	}
	if _, err := ParseMaxErrors("nope"); err == nil {
		t.Error("an unparseable max_errors was accepted")
	} else if !strings.Contains(err.Error(), MaxErrorsNone) {
		t.Errorf("the refusal %q does not name %q", err, MaxErrorsNone)
	}
}
