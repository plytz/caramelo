package place

import (
	"path/filepath"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const (
	CanonicalFresh = "fresh"

	CanonicalCommander = "commander"

	CanonicalCommanderCheckout = "commander-checkout"

	CanonicalHub = "hub"

	CanonicalMember = "member"
)

func CanonicalNames() []string {
	return []string{CanonicalFresh, CanonicalCommander, CanonicalCommanderCheckout, CanonicalHub, CanonicalMember}
}

func Canonical(name string) (Context, bool) {
	switch name {
	case CanonicalFresh:
		return freshBox(), true
	case CanonicalCommander:
		return commanderOutsideACheckout(), true
	case CanonicalCommanderCheckout:
		return commanderInACheckout(), true
	case CanonicalHub:
		return canonicalHub(), true
	case CanonicalMember:
		return canonicalMember(), true
	}
	return Context{}, false
}

func Canonicals() []Context {
	out := make([]Context, 0, len(CanonicalNames()))
	for _, name := range CanonicalNames() {
		c, _ := Canonical(name)
		out = append(out, c)
	}
	return out
}

const (
	canonicalHome = "/home/alex"

	canonicalWorktree = canonicalHome + "/src/shop-feat-x"
)

func freshBox() Context {
	return Context{
		Role:            RoleFresh,
		ServerConfigDir: serverconfig.DefaultConfigDir,
		CommanderConfig: canonicalCommanderConfig(),
		TalksTo: TalksTo{
			Kind:    TalksNothing,
			Socket:  serverconfig.Default().SocketPath(),
			Problem: FreshBoxProblem,
		},
		Work: Work{Dir: canonicalHome},
	}
}

func canonicalCommanderConfig() string {
	return filepath.Join(canonicalHome, ".config", remote.CommanderDirName, remote.CommanderConfigFile)
}

func canonicalCommander() *Commander {
	dir := filepath.Dir(canonicalCommanderConfig())
	return &Commander{
		DefaultFleet: "home",
		IdentityKey:  filepath.Join(dir, vpnclient.IdentityKeyFile),
		Identified:   true,
		Records:      filepath.Join(dir, vpnclient.KeyDir),
		CacheDir:     filepath.Join(canonicalHome, ".cache", remote.CommanderDirName),
		Fleets: []Fleet{
			{
				Name:      "home",
				Hub:       "caramelo@box.example.com:4022",
				PublicKey: "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
				Apps:      []string{"blog", "shop"},
				Default:   true,
				Record:    true,
			},
			{Name: "work", Hub: "ops@hub.work.example:4022"},
		},
	}
}

func commanderOutsideACheckout() Context {
	return Context{
		Name:            "laptop",
		Role:            RoleCommander,
		ConfigFile:      canonicalCommanderConfig(),
		ServerConfigDir: serverconfig.DefaultConfigDir,
		CommanderConfig: canonicalCommanderConfig(),
		Commander:       canonicalCommander(),
		TalksTo: TalksTo{
			Kind:   TalksSSH,
			Fleet:  "home",
			Target: "caramelo@box.example.com:4022",
			Why:    "commander.default_fleet",
		},
		Work: Work{Dir: canonicalHome},
	}
}

func commanderInACheckout() Context {
	c := commanderOutsideACheckout()
	c.TalksTo.Why = "the fleet recorded for app shop"
	c.Work = Work{
		Dir:     canonicalWorktree,
		App:     "shop",
		AppFrom: FromCheckout,
		Env:     "feat-x",
		EnvFrom: FromWorktree,
	}
	return c
}

func canonicalHub() Context {
	cfg := serverconfig.Default()
	cfg.Name, cfg.Role = "box", serverconfig.RoleHub
	cfg.Hub = serverconfig.Hub{Fleet: "home", Range: serverconfig.DefaultFleetRange}
	cfg.Edge = true
	return serverContext(cfg)
}

func canonicalMember() Context {
	cfg := serverconfig.Default()
	cfg.Name, cfg.Role = "worker", serverconfig.RoleMember
	cfg.Member = serverconfig.Member{
		Fleet:  "home",
		Subnet: "10.81.0.0/16",
		Hub: serverconfig.MemberHub{
			Name:      "box",
			Endpoint:  "box.example.com:4021",
			Address:   "10.80.0.1",
			PublicKey: "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
		},
	}
	cfg.VPNSubnet = cfg.Member.Subnet
	return serverContext(cfg)
}

func serverContext(cfg serverconfig.Config) Context {
	dir := serverconfig.DefaultConfigDir
	running := func(path string) bool { return path == cfg.SocketPath() || (cfg.Edge && path == cfg.EdgeSocketPath()) }
	return Context{
		Name:            cfg.Name,
		Role:            cfg.FleetRole(),
		ConfigFile:      serverconfig.Path(dir),
		ServerConfigDir: dir,
		Server:          serverOf(cfg, dir, running),
		TalksTo: TalksTo{
			Kind:   TalksSocket,
			Socket: cfg.SocketPath(),
			Why:    "local daemon socket",
		},
		Work: Work{Dir: "/root"},
	}
}
