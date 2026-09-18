package place

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const hubConfig = "name: box\nrole: hub\nhub:\n  fleet: home\nedge: true\nacme_email: ops@example.com\n"

const memberConfig = `name: worker
role: member
vpn_subnet: 10.81.0.0/16
member:
  fleet: home
  subnet: 10.81.0.0/16
  hub:
    endpoint: box.example.com:4021
    address: 10.80.0.1
    public_key: Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=
`

func serverDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(serverconfig.Path(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverconfig.ConfigDirEnv, dir)
	return dir
}

func noServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(serverconfig.ConfigDirEnv, dir)
	return dir
}

func commanderHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	return filepath.Join(home, remote.CommanderDirName)
}

func saveCommander(t *testing.T, c remote.CommanderConfig) string {
	t.Helper()
	dir := commanderHome(t)
	path := filepath.Join(dir, remote.CommanderConfigFile)
	if err := remote.SaveCommanderConfigTo(path, c); err != nil {
		t.Fatal(err)
	}
	return path
}

func oneFleet() remote.CommanderConfig {
	return remote.CommanderConfig{
		Name: "laptop",
		Role: remote.RoleCommander,
		Commander: remote.Commander{
			DefaultFleet: "home",
			Fleets: map[string]remote.Fleet{
				"home": {Hub: "caramelo@box.example.com:4022", Apps: []string{"shop"}},
			},
		},
	}
}

func twoFleets() remote.CommanderConfig {
	c := oneFleet()
	c.Commander.DefaultFleet = ""
	c.Commander.Fleets["work"] = remote.Fleet{Hub: "ops@hub.work.example:4022", Apps: []string{"blog"}}
	return c
}

func detect(t *testing.T, o Options) Context {
	t.Helper()
	if o.Git == nil {
		o.Git = fakeGit(nil)
	}
	if o.SocketExists == nil {
		o.SocketExists = func(string) bool { return false }
	}
	c, err := Detect(context.Background(), o)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return c
}

func TestAFreshBoxIsNobodyAndSaysWhereItLooked(t *testing.T) {
	dir := noServer(t)
	home := commanderHome(t)

	c := detect(t, Options{})
	if !c.IsFresh() || c.Role != RoleFresh {
		t.Errorf("role = %q, want %q", c.Role, RoleFresh)
	}
	if c.ServerConfigDir != dir || c.ConfigFile != "" {
		t.Errorf("server config dir = %q, config read = %q", c.ServerConfigDir, c.ConfigFile)
	}
	if want := filepath.Join(home, remote.CommanderConfigFile); c.CommanderConfig != want {
		t.Errorf("commander config = %q, want %q", c.CommanderConfig, want)
	}
	if c.Commander != nil || c.Server != nil {
		t.Errorf("a fresh box holds nothing: %+v", c)
	}
	if c.TalksTo.Kind != TalksNothing || c.TalksTo.Problem != FreshBoxProblem {
		t.Errorf("talks to = %+v, want nothing and the fresh box's own problem", c.TalksTo)
	}
}

func TestAServerConfigSaysWhatThisMachineIs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		role     string
		machine  string
		fleet    string
		endpoint string
		edge     bool
	}{
		{"a hub", hubConfig, RoleHub, "box", "home", "", true},
		{"a member", memberConfig, RoleMember, "worker", "home", "box.example.com:4021", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := serverDir(t, tc.body)
			commanderHome(t)
			c := detect(t, Options{})

			if c.Role != tc.role || c.Name != tc.machine {
				t.Errorf("%s says it is %q named %q, want %q named %q", dir, c.Role, c.Name, tc.role, tc.machine)
			}
			if !c.IsServer() {
				t.Fatal("a machine with a config.yaml is a server")
			}
			if c.ConfigFile != serverconfig.Path(dir) {
				t.Errorf("config read = %q, want %q", c.ConfigFile, serverconfig.Path(dir))
			}
			if c.Server.Fleet != tc.fleet {
				t.Errorf("fleet = %q, want %q", c.Server.Fleet, tc.fleet)
			}
			if tc.endpoint == "" {
				if c.Server.Hub != nil {
					t.Errorf("hub = %+v, want none on a hub", c.Server.Hub)
				}
			} else if c.Server.Hub == nil || c.Server.Hub.Endpoint != tc.endpoint {
				t.Errorf("hub = %+v, want the endpoint %q", c.Server.Hub, tc.endpoint)
			}
			if c.Server.Services.Edge.Enabled != tc.edge {
				t.Errorf("edge = %v, want %v", c.Server.Services.Edge.Enabled, tc.edge)
			}
			if c.Commander != nil {
				t.Errorf("a server holds no commander: %+v", c.Commander)
			}
		})
	}
}

func TestAServerCarriesItsPathsAndServices(t *testing.T) {
	dir := serverDir(t, hubConfig)
	commanderHome(t)
	cfg, err := serverconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := detect(t, Options{SocketExists: func(p string) bool { return p == cfg.SocketPath() }})

	want := Paths{
		Config: dir, State: cfg.StateDir, Data: cfg.DataDir, Run: cfg.RunDir,
		Socket: cfg.SocketPath(), Apps: cfg.AppsDir(), Edge: cfg.EdgeDir(),
		VPN: cfg.VPNDir(), Secrets: cfg.SecretsDir(),
	}
	if c.Server.Paths != want {
		t.Errorf("paths = %+v, want %+v", c.Server.Paths, want)
	}
	if c.Server.User != cfg.User || c.Server.Group != cfg.Group {
		t.Errorf("user = %s:%s, want %s:%s", c.Server.User, c.Server.Group, cfg.User, cfg.Group)
	}
	if !c.Server.Services.Daemon.Present || c.Server.Services.Edge.Socket.Present {
		t.Errorf("services = %+v, want the daemon socket there and the edge socket not", c.Server.Services)
	}
	if c.Server.Services.VPNListen != cfg.VPNListen || c.Server.Services.APIListen != cfg.APIListen {
		t.Errorf("services = %+v, want vpn_listen %q and api_listen %q",
			c.Server.Services, cfg.VPNListen, cfg.APIListen)
	}
	if c.TalksTo.Kind != TalksSocket || c.TalksTo.Socket != cfg.SocketPath() {
		t.Errorf("talks to = %+v, want the socket of this machine", c.TalksTo)
	}
}

func TestACommanderIsReadFromTheUsersOwnConfig(t *testing.T) {
	noServer(t)
	path := saveCommander(t, oneFleet())
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "identity.key"), []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := detect(t, Options{})
	if !c.IsCommander() || c.Name != "laptop" || c.ConfigFile != path {
		t.Fatalf("context = %+v, want the commander laptop read from %s", c, path)
	}
	if c.Commander.DefaultFleet != "home" || len(c.Commander.Fleets) != 1 {
		t.Errorf("commander = %+v, want one fleet and home as the default", c.Commander)
	}
	f := c.Commander.Fleets[0]
	if f.Name != "home" || f.Hub != "caramelo@box.example.com:4022" || !f.Default || f.Record {
		t.Errorf("fleet = %+v, want home as the default with no tunnel record yet", f)
	}
	if !c.Commander.Identified || c.Commander.IdentityKey != filepath.Join(dir, "identity.key") {
		t.Errorf("identity = %q (present %v), want the key beside the config",
			c.Commander.IdentityKey, c.Commander.Identified)
	}
	if c.Commander.Records != filepath.Join(dir, "vpn") {
		t.Errorf("records = %q, want the vpn directory beside the config", c.Commander.Records)
	}
	if c.Server != nil {
		t.Errorf("a commander holds no server: %+v", c.Server)
	}
}

func TestAServerConfigBeatsTheCommanderConfig(t *testing.T) {
	serverDir(t, hubConfig)
	saveCommander(t, oneFleet())

	c := detect(t, Options{})
	if c.Role != RoleHub || c.Commander != nil {
		t.Errorf("context = %+v, want the machine's own config to win", c)
	}
}

func TestUnderSudoTheCommanderConfigIsTheInvokingUsers(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user to look up")
	}
	noServer(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", me.Username)

	c := detect(t, Options{})
	want := filepath.Join(me.HomeDir, ".config", remote.CommanderDirName, remote.CommanderConfigFile)
	if c.CommanderConfig != want {
		t.Errorf("commander config = %q, want %q: under sudo the config read is the invoking user's",
			c.CommanderConfig, want)
	}
}

func TestABrokenConfigIsAProblemAndNotAFailure(t *testing.T) {
	t.Run("a server config", func(t *testing.T) {
		serverDir(t, "name: box\nrole: hub\nhub:\n  fleet: [oops\n")
		commanderHome(t)
		c := detect(t, Options{})
		if c.Role != RoleUnknown || !c.IsServer() {
			t.Errorf("role = %q, want %q on a machine whose config cannot be read", c.Role, RoleUnknown)
		}
		if !strings.Contains(c.Problem, "yaml") {
			t.Errorf("problem = %q, want the parser's own words", c.Problem)
		}
		if c.Server.Paths.State != serverconfig.DefaultStateDir {
			t.Errorf("paths = %+v, want the defaults when the file says nothing", c.Server.Paths)
		}
	})

	t.Run("a server config that does not validate", func(t *testing.T) {
		serverDir(t, "name: box\nrole: hub\n")
		commanderHome(t)
		c := detect(t, Options{})
		if c.Role != RoleHub {
			t.Errorf("role = %q, want %q: the file says what it is even when it is incomplete", c.Role, RoleHub)
		}
		if !strings.Contains(c.Problem, "hub.fleet") {
			t.Errorf("problem = %q, want it to name the missing key", c.Problem)
		}
	})

	t.Run("a commander config", func(t *testing.T) {
		noServer(t)
		dir := commanderHome(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, remote.CommanderConfigFile)
		if err := os.WriteFile(path, []byte("machines:\n  box: alex@box:4022\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := detect(t, Options{})
		if !c.IsCommander() {
			t.Errorf("role = %q, want a commander: the file is there even though it cannot be read", c.Role)
		}
		if !strings.Contains(c.Problem, "retired") {
			t.Errorf("problem = %q, want the loader's own words", c.Problem)
		}
	})
}

func TestTheWorkingDirectoryNamesTheAppAndTheEnvironment(t *testing.T) {
	noServer(t)
	commanderHome(t)
	worktree := fakeGit(map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	})

	c := detect(t, Options{Git: worktree})
	if c.Work.App != "shop" || c.Work.AppFrom != FromCheckout {
		t.Errorf("app = %q from %q, want shop from the checkout", c.Work.App, c.Work.AppFrom)
	}
	if c.Work.Env != "feat-x" || c.Work.EnvFrom != FromWorktree {
		t.Errorf("env = %q from %q, want feat-x from the worktree", c.Work.Env, c.Work.EnvFrom)
	}
	if !filepath.IsAbs(c.Work.Dir) {
		t.Errorf("dir = %q, want an absolute path", c.Work.Dir)
	}

	t.Setenv("CARAMELO_APP", "blog")
	t.Setenv("CARAMELO_ENV", "main")
	c = detect(t, Options{Git: worktree})
	if c.Work.App != "blog" || c.Work.AppFrom != FromAppEnv {
		t.Errorf("app = %q from %q, want blog from CARAMELO_APP", c.Work.App, c.Work.AppFrom)
	}
	if c.Work.Env != "main" || c.Work.EnvFrom != FromEnvEnv {
		t.Errorf("env = %q from %q, want main from CARAMELO_ENV", c.Work.Env, c.Work.EnvFrom)
	}

	c = detect(t, Options{Git: worktree, App: "store", Env: "feat-y"})
	if c.Work.App != "store" || c.Work.AppFrom != FromFlag || c.Work.Env != "feat-y" {
		t.Errorf("work = %+v, want what the flags say", c.Work)
	}
}

func TestTheEnvironmentOverrideSpendsNoGitCall(t *testing.T) {
	noServer(t)
	commanderHome(t)
	t.Setenv("CARAMELO_APP", "blog")
	t.Setenv("CARAMELO_ENV", "main")
	var calls int
	detect(t, Options{Git: countingGit(nil, &calls)})
	if calls != 0 {
		t.Errorf("git was asked %d times although the app and the environment were both named", calls)
	}
}

func TestTheFleetACommandWouldTalkToAndWhy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    remote.CommanderConfig
		opts   Options
		fleet  string
		reason string
	}{
		{"--fleet", twoFleets(), Options{Fleet: "work"}, "work", "--fleet work"},
		{"the default fleet", oneFleet(), Options{}, "home", "commander.default_fleet"},
		{"the only fleet", func() remote.CommanderConfig {
			c := oneFleet()
			c.Commander.DefaultFleet = ""
			return c
		}(), Options{}, "home", "the only fleet in the commander config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noServer(t)
			saveCommander(t, tc.cfg)
			c := detect(t, tc.opts)
			if c.TalksTo.Kind != TalksSSH || c.TalksTo.Fleet != tc.fleet || c.TalksTo.Why != tc.reason {
				t.Errorf("talks to = %+v, want fleet %s because of %q", c.TalksTo, tc.fleet, tc.reason)
			}
		})
	}

	t.Run("the fleet recorded for the app of this checkout", func(t *testing.T) {
		noServer(t)
		saveCommander(t, twoFleets())
		git := fakeGit(map[string]string{
			"rev-parse --git-common-dir": "/mnt/caramelo/apps/blog/repo.git",
			"rev-parse --show-toplevel":  "/mnt/caramelo/apps/blog/envs/feat-x/src",
		})
		c := detect(t, Options{Git: git})
		if c.TalksTo.Fleet != "work" || !strings.Contains(c.TalksTo.Why, "blog") {
			t.Errorf("talks to = %+v, want the fleet recorded for blog", c.TalksTo)
		}
	})

	t.Run("several fleets and nothing to pick one", func(t *testing.T) {
		noServer(t)
		saveCommander(t, twoFleets())
		c := detect(t, Options{})
		if c.TalksTo.Kind != TalksNothing || !strings.Contains(c.TalksTo.Problem, "home, work") {
			t.Errorf("talks to = %+v, want a refusal naming the fleets", c.TalksTo)
		}
	})

	t.Run("a raw machine", func(t *testing.T) {
		noServer(t)
		saveCommander(t, oneFleet())
		c := detect(t, Options{Machine: "alex@10.0.0.5:4023"})
		if c.TalksTo.Kind != TalksSSH || c.TalksTo.Fleet != "" || c.TalksTo.Target != "alex@10.0.0.5:4023" {
			t.Errorf("talks to = %+v, want the raw target and no fleet", c.TalksTo)
		}
	})

	t.Run("a commander with no fleet at all", func(t *testing.T) {
		noServer(t)
		saveCommander(t, remote.CommanderConfig{Name: "laptop", Role: remote.RoleCommander})
		c := detect(t, Options{})
		if c.TalksTo.Kind != TalksNothing || !strings.Contains(c.TalksTo.Problem, "fleet add") {
			t.Errorf("talks to = %+v, want a refusal naming 'fleet add'", c.TalksTo)
		}
	})
}

func TestDetectAsksGitTwiceAtMost(t *testing.T) {
	noServer(t)
	saveCommander(t, oneFleet())
	var calls int
	detect(t, Options{Git: countingGit(map[string]string{
		"rev-parse --git-common-dir": "/mnt/caramelo/apps/shop/repo.git",
		"rev-parse --show-toplevel":  "/mnt/caramelo/apps/shop/envs/feat-x/src",
	}, &calls)})
	if calls > 2 {
		t.Errorf("Detect asked git %d times; a context costs one git rev-parse, twice at most", calls)
	}
}

func TestDetectIsCheapEnoughToRunBeforeEveryCommand(t *testing.T) {
	serverDir(t, hubConfig)
	commanderHome(t)
	o := Options{Git: fakeGit(nil), SocketExists: func(string) bool { return false }}

	const runs = 50
	start := time.Now()
	for i := 0; i < runs; i++ {
		if _, err := Detect(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	each := time.Since(start) / runs
	if each > 10*time.Millisecond {
		t.Errorf("Detect took %s each; it runs before every command and must stay in the low milliseconds", each)
	}
}

func TestEveryCanonicalContextIsOneOfTheFiveAndHasAHeader(t *testing.T) {
	names := CanonicalNames()
	if len(names) != 5 {
		t.Fatalf("canonical contexts = %v, want the five places of the design", names)
	}
	roles := map[string]bool{}
	for _, name := range names {
		c, ok := Canonical(name)
		if !ok {
			t.Fatalf("Canonical(%q) is missing", name)
		}
		roles[c.Role] = true
		head := c.Header()
		if head == "" || strings.Contains(head, " ·  · ") || strings.HasSuffix(head, "·") {
			t.Errorf("%s header = %q", name, head)
		}
	}
	for _, role := range []string{RoleFresh, RoleCommander, RoleHub, RoleMember} {
		if !roles[role] {
			t.Errorf("no canonical context is a %s", role)
		}
	}
	if _, ok := Canonical("nowhere"); ok {
		t.Error("Canonical answered for a name that is not one of the five")
	}
}

func TestACanonicalContextLooksLikeADetectedOne(t *testing.T) {
	dir := serverDir(t, hubConfig)
	commanderHome(t)
	cfg, err := serverconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := detect(t, Options{SocketExists: func(p string) bool { return p == cfg.SocketPath() }})
	want, _ := Canonical(CanonicalHub)

	if got.Role != want.Role || got.Name != want.Name {
		t.Errorf("detected %s named %s, want a %s named %s", got.Role, got.Name, want.Role, want.Name)
	}
	if got.Server.Paths.State != want.Server.Paths.State || got.Server.Fleet != want.Server.Fleet {
		t.Errorf("detected %+v, want the canonical hub %+v", got.Server, want.Server)
	}
	if got.Server.Services.Daemon.Present != want.Server.Services.Daemon.Present {
		t.Errorf("caramelod = %+v, want %+v", got.Server.Services.Daemon, want.Server.Services.Daemon)
	}
}
