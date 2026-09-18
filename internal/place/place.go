package place

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/userdir"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const (
	RoleFresh = "fresh"

	RoleCommander = serverconfig.RoleCommander

	RoleHub = serverconfig.RoleHub

	RoleMember = serverconfig.RoleMember

	RoleUnknown = "unknown"
)

const (
	TalksSocket = "socket"

	TalksSSH = "ssh"

	TalksNothing = "nothing"
)

const (
	FromCheckout = "the checkout"

	FromWorktree = "the worktree"

	FromAppEnv = "CARAMELO_APP"

	FromEnvEnv = "CARAMELO_ENV"

	FromFlag = "the flag"
)

type Context struct {
	Name string `json:"name,omitempty"`

	Role string `json:"role"`

	ConfigFile string `json:"config_file,omitempty"`

	ServerConfigDir string `json:"server_config_dir"`

	CommanderConfig string `json:"commander_config,omitempty"`

	Problem string `json:"problem,omitempty"`

	Commander *Commander `json:"commander,omitempty"`

	Server *Server `json:"server,omitempty"`

	TalksTo TalksTo `json:"talks_to"`

	Work Work `json:"work"`
}

type Commander struct {
	Fleets []Fleet `json:"fleets"`

	DefaultFleet string `json:"default_fleet,omitempty"`

	IdentityKey string `json:"identity_key"`

	Identified bool `json:"identified"`

	Records string `json:"records"`

	CacheDir string `json:"cache_dir,omitempty"`
}

type Fleet struct {
	Name string `json:"name"`

	Hub string `json:"hub"`

	PublicKey string `json:"public_key,omitempty"`

	Apps []string `json:"apps,omitempty"`

	Default bool `json:"default"`

	Record bool `json:"record"`
}

type TalksTo struct {
	Kind string `json:"kind"`

	Fleet string `json:"fleet,omitempty"`

	Target string `json:"target,omitempty"`

	Socket string `json:"socket,omitempty"`

	Why string `json:"why,omitempty"`

	Problem string `json:"problem,omitempty"`
}

type Server struct {
	Fleet string `json:"fleet,omitempty"`

	Hub *MemberHub `json:"hub,omitempty"`

	User string `json:"user"`

	Group string `json:"group"`

	Paths Paths `json:"paths"`

	Services Services `json:"services"`
}

type MemberHub struct {
	Endpoint string `json:"endpoint,omitempty"`

	Address string `json:"address,omitempty"`

	PublicKey string `json:"public_key,omitempty"`
}

type Paths struct {
	Config string `json:"config"`

	State string `json:"state"`

	Data string `json:"data"`

	Run string `json:"run"`

	Socket string `json:"socket"`

	Apps string `json:"apps"`

	Edge string `json:"edge"`

	VPN string `json:"vpn"`

	Secrets string `json:"secrets"`
}

type Services struct {
	Daemon Socket `json:"caramelod"`

	Edge Edge `json:"edge"`

	VPNListen string `json:"vpn_listen,omitempty"`

	APIListen string `json:"api_listen,omitempty"`
}

type Socket struct {
	Path string `json:"path"`

	Present bool `json:"present"`
}

type Edge struct {
	Enabled bool `json:"enabled"`

	Socket Socket `json:"socket"`
}

type Work struct {
	Dir string `json:"dir"`

	App string `json:"app,omitempty"`

	AppFrom string `json:"app_from,omitempty"`

	Env string `json:"env,omitempty"`

	EnvFrom string `json:"env_from,omitempty"`
}

type Options struct {
	ConfigDir string

	Dir string

	Git GitFunc

	SocketExists func(path string) bool

	Fleet string

	Machine string

	App string

	Env string
}

func Detect(ctx context.Context, o Options) (Context, error) {
	if strings.TrimSpace(o.ConfigDir) == "" {
		o.ConfigDir = serverconfig.ConfigDir()
	}
	if strings.TrimSpace(o.Dir) == "" {
		o.Dir = "."
	}
	if o.SocketExists == nil {
		o.SocketExists = remote.SocketExists
	}

	c := Context{Role: RoleFresh, ServerConfigDir: o.ConfigDir}
	c.Work = o.work(ctx)

	commander := remote.CommanderConfig{}
	switch {
	case serverconfig.Exists(o.ConfigDir):
		o.server(&c)
	default:
		cfg, err := o.commander(&c)
		if err != nil {
			return Context{}, err
		}
		commander = cfg
	}
	c.TalksTo = o.talksTo(&c, commander)
	return c, nil
}

func (o Options) work(ctx context.Context) Work {
	w := Work{Dir: workDir(o.Dir)}
	w.App, w.AppFrom = named(o.App, FromAppEnv)
	w.Env, w.EnvFrom = named(o.Env, FromEnvEnv)
	if w.App != "" && w.Env != "" {
		return w
	}
	app, environment := Checkout(ctx, o.Git, o.Dir)
	if w.App == "" && app != "" {
		w.App, w.AppFrom = app, FromCheckout
	}
	if w.Env == "" && environment != "" {
		w.Env, w.EnvFrom = environment, FromWorktree
	}
	return w
}

func named(flag, key string) (string, string) {
	if v := strings.TrimSpace(flag); v != "" {
		return v, FromFlag
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, key
	}
	return "", ""
}

func workDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func (o Options) server(c *Context) {
	c.ConfigFile = serverconfig.Path(o.ConfigDir)
	cfg, err := serverconfig.Load(o.ConfigDir)
	if err != nil {
		c.Problem = problemWith(c.ConfigFile, err)
		if strings.TrimSpace(cfg.Role) == "" {
			name, role := probeNameAndRole(c.ConfigFile)
			cfg = serverconfig.Default()
			cfg.Name, cfg.Role = name, role
		}
	}

	c.Name = cfg.Name
	c.Role = RoleUnknown
	switch strings.ToLower(strings.TrimSpace(cfg.Role)) {
	case RoleHub:
		c.Role = RoleHub
	case RoleMember:
		c.Role = RoleMember
	}

	c.Server = serverOf(cfg, o.ConfigDir, o.SocketExists)
}

func serverOf(cfg serverconfig.Config, dir string, exists func(string) bool) *Server {
	s := &Server{
		Fleet: cfg.FleetName(),
		User:  cfg.User,
		Group: cfg.Group,
		Paths: Paths{
			Config:  dir,
			State:   cfg.StateDir,
			Data:    cfg.DataDir,
			Run:     cfg.RunDir,
			Socket:  cfg.SocketPath(),
			Apps:    cfg.AppsDir(),
			Edge:    cfg.EdgeDir(),
			VPN:     cfg.VPNDir(),
			Secrets: cfg.SecretsDir(),
		},
		Services: Services{
			Daemon:    socketAt(cfg.SocketPath(), exists),
			Edge:      Edge{Enabled: cfg.Edge, Socket: socketAt(cfg.EdgeSocketPath(), exists)},
			VPNListen: cfg.VPNListen,
			APIListen: cfg.APIListen,
		},
	}
	if cfg.IsMember() {
		h := cfg.Member.Hub
		s.Hub = &MemberHub{Endpoint: h.Endpoint, Address: h.Address, PublicKey: h.PublicKey}
	}
	return s
}

func problemWith(path string, err error) string {
	msg := err.Error()
	if strings.Contains(msg, path) {
		return msg
	}
	return path + ": " + msg
}

func probeNameAndRole(path string) (name, role string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var doc struct {
		Name string `yaml:"name"`
		Role string `yaml:"role"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", ""
	}
	return strings.TrimSpace(doc.Name), strings.TrimSpace(doc.Role)
}

func socketAt(path string, exists func(string) bool) Socket {
	return Socket{Path: path, Present: path != "" && exists(path)}
}

func (o Options) commander(c *Context) (remote.CommanderConfig, error) {
	path, err := remote.CommanderConfigPath()
	if err != nil {
		return remote.CommanderConfig{}, fmt.Errorf("locate the commander config: %w", err)
	}
	c.CommanderConfig = path

	if !fileExists(path) {
		return remote.CommanderConfig{}, nil
	}

	c.Role, c.ConfigFile = RoleCommander, path
	cfg, err := remote.LoadCommanderConfigFrom(path)
	if err != nil {
		c.Problem = problemWith(path, err)
		cfg = remote.CommanderConfig{}
	}
	c.Name = cfg.MachineName()

	dir := filepath.Dir(path)
	records := filepath.Join(dir, vpnclient.KeyDir)
	identity := filepath.Join(dir, vpnclient.IdentityKeyFile)
	store := &vpnclient.FileRecordStore{Dir: records}
	cm := &Commander{
		DefaultFleet: cfg.Commander.DefaultFleet,
		IdentityKey:  identity,
		Identified:   fileExists(identity),
		Records:      records,
		Fleets:       []Fleet{},
	}
	if cache, err := userdir.Cache(); err == nil {
		cm.CacheDir = filepath.Join(cache, remote.CommanderDirName)
	}
	for _, name := range cfg.FleetNames() {
		f := cfg.Commander.Fleets[name]
		cm.Fleets = append(cm.Fleets, Fleet{
			Name:      name,
			Hub:       f.Hub,
			PublicKey: f.PublicKey,
			Apps:      f.Apps,
			Default:   cfg.Commander.DefaultFleet == name,
			Record:    fileExists(store.Path(name)),
		})
	}
	c.Commander = cm
	return cfg, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (o Options) talksTo(c *Context, cfg remote.CommanderConfig) TalksTo {
	socket := serverconfig.Default().SocketPath()
	user := serverconfig.Default().User
	if c.Server != nil {
		socket, user = c.Server.Paths.Socket, c.Server.User
	}
	sel := remote.Selection{
		Machine:      o.Machine,
		Fleet:        o.Fleet,
		App:          func() string { return c.Work.App },
		Config:       cfg,
		SocketPath:   socket,
		SocketUser:   user,
		SocketExists: o.SocketExists,
	}
	p, err := sel.Plan()
	switch {
	case errors.Is(err, remote.ErrNoFleet):
		return TalksTo{Kind: TalksNothing, Socket: socket, Problem: noFleetHere(c)}
	case err != nil:
		return TalksTo{Kind: TalksNothing, Socket: socket, Problem: err.Error()}
	case p.Kind == remote.KindSocket:
		return TalksTo{Kind: TalksSocket, Socket: socket, Why: p.Why}
	}
	return TalksTo{Kind: TalksSSH, Fleet: p.Fleet, Target: p.Target.String(), Why: p.Why}
}

const FreshBoxProblem = "this box has no config at all, and 'caramelo commander init' " +
	"is the one command it can run"

func noFleetHere(c *Context) string {
	switch {
	case c.IsServer():
		return fmt.Sprintf("no caramelod socket at %s: the daemon of this machine is not answering",
			c.Server.Paths.Socket)
	case c.IsFresh():
		return FreshBoxProblem
	}
	return "no fleet in the commander config: record one with 'caramelo fleet add NAME user@host', " +
		"or make this machine a hub with 'sudo caramelo hub setup'"
}

func (c Context) ServerConfigFile() string { return serverconfig.Path(c.ServerConfigDir) }

func (c Context) IsFresh() bool { return c.Role == RoleFresh }

func (c Context) IsCommander() bool { return c.Role == RoleCommander }

func (c Context) IsServer() bool { return c.Server != nil }

func (c Context) IsHub() bool { return c.Role == RoleHub }

func (c Context) IsMember() bool { return c.Role == RoleMember }

func (c Context) FleetNames() []string {
	if c.Commander == nil {
		return nil
	}
	names := make([]string, 0, len(c.Commander.Fleets))
	for _, f := range c.Commander.Fleets {
		names = append(names, f.Name)
	}
	return names
}

func (c Context) Header() string {
	switch {
	case c.IsFresh():
		return "fresh box: no config · run 'caramelo commander init' to name this machine"
	case c.IsServer():
		return strings.Join(c.serverHeader(), " · ")
	case c.IsCommander():
		return strings.Join(c.commanderHeader(), " · ")
	}
	return strings.Join([]string{c.Name + ", " + c.Role, c.ConfigFile}, " · ")
}

func (c Context) serverHeader() []string {
	s := c.Server
	what := c.Role
	if c.Role == RoleUnknown {
		what = "a machine whose config cannot be read"
	}
	if s.Fleet != "" {
		what += " of fleet " + s.Fleet
	}
	if c.Role == RoleMember && s.Hub != nil && s.Hub.Endpoint != "" {
		what += " (hub " + s.Hub.Endpoint + ")"
	}
	name := c.Name
	if name == "" {
		name = "unnamed machine"
	}
	parts := []string{name + ", " + what, c.ConfigFile}
	if s.Services.Daemon.Present {
		parts = append(parts, "caramelod socket "+s.Services.Daemon.Path)
	} else {
		parts = append(parts, "no caramelod socket")
	}
	if c.Role != RoleMember {
		parts = append(parts, "state "+s.Paths.State, "data "+s.Paths.Data)
	}
	return append(parts, "edge "+onOff(s.Services.Edge.Enabled))
}

func (c Context) commanderHeader() []string {
	parts := []string{c.Name + ", " + RoleCommander, c.fleetsClause()}
	where := "no app checkout here"
	if c.Work.App != "" {
		where = "app " + c.Work.App
	}
	switch {
	case c.TalksTo.Kind == TalksSSH && c.TalksTo.Fleet != "":
		where += fmt.Sprintf(" → fleet %s (%s)", c.TalksTo.Fleet, c.TalksTo.Why)
	case c.TalksTo.Kind == TalksSSH:
		where += fmt.Sprintf(" → %s (%s)", c.TalksTo.Target, c.TalksTo.Why)
	case c.TalksTo.Kind == TalksSocket:
		where += " → the caramelod on this machine"
	}
	parts = append(parts, where)
	if c.Work.Env != "" {
		parts = append(parts, "env "+c.Work.Env)
	}
	return parts
}

func (c Context) fleetsClause() string {
	if c.Commander == nil || len(c.Commander.Fleets) == 0 {
		return "no fleets"
	}
	names := make([]string, 0, len(c.Commander.Fleets))
	for _, f := range c.Commander.Fleets {
		if f.Default {
			names = append(names, f.Name+" (default)")
			continue
		}
		names = append(names, f.Name)
	}
	return "fleets " + strings.Join(names, ", ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
