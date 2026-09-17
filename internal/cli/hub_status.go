package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup"
)

func init() {
	registerHub(func(a *app) *cobra.Command { return a.hubStatusCmd() })
}

type hubStatus struct {
	Version     string       `json:"version"`
	Hostname    string       `json:"hostname"`
	ConfigFile  string       `json:"config_file"`
	Installed   bool         `json:"installed"`
	User        userStatus   `json:"user"`
	Caramelod   unitStatus   `json:"caramelod"`
	Docker      unitStatus   `json:"docker"`
	Socket      socketStatus `json:"socket"`
	Port        portStatus   `json:"port"`
	Paths       api.Paths    `json:"paths"`
	Swap        swapStatus   `json:"swap"`
	BinaryFound bool         `json:"binary_found"`
}

type swapStatus struct {
	TotalBytes int64  `json:"total_bytes"`
	Managed    bool   `json:"managed"`
	Backend    string `json:"backend"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

type userStatus struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	UID    int    `json:"uid,omitempty"`
	Home   string `json:"home,omitempty"`
}

type unitStatus struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type socketStatus struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Mode    string `json:"mode,omitempty"`
}

type portStatus struct {
	Port      int  `json:"port"`
	Listening bool `json:"listening"`

	APIListen string `json:"api_listen,omitempty"`

	VPNPort      int  `json:"vpn_port,omitempty"`
	VPNListening bool `json:"vpn_listening"`
}

func (a *app) hubStatusCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report what setup installed on this machine and whether it is running",
		Long: `Report this machine's Caramelo installation: the configuration, the user,
the caramelod and Docker user units, the local socket and the API port.

It reads the machine directly and never talks to caramelod, so it works when the
daemon is down. It exits 1 when caramelod is not active.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			st := hubStatusOf(cmd.Context(), runner.Exec{}, configDir)
			if err := a.printer().Result(st, func(w io.Writer) error {
				return writeHubStatus(w, st)
			}); err != nil {
				return err
			}
			if !st.Caramelod.Active {
				return &exitError{ExitError}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configDir, "config-dir", serverconfig.DefaultConfigDir, "directory holding config.yaml")
	return cmd
}

func hubStatusOf(ctx context.Context, run runner.Runner, configDir string) hubStatus {
	cfg := serverconfig.Default()
	st := hubStatus{
		Version:    version,
		ConfigFile: serverconfig.Path(configDir),
	}
	if serverconfig.Exists(configDir) {
		if loaded, err := serverconfig.Load(configDir); err == nil {
			cfg, st.Installed = loaded, true
		}
	}
	st.Hostname, _ = os.Hostname()
	st.Paths = api.Paths{Config: configDir, State: cfg.StateDir, Data: cfg.DataDir, Socket: cfg.SocketPath()}
	st.User = userStatusOf(cfg.User)
	st.Caramelod = userUnitStatus(ctx, run, cfg.User, setup.UserUnit, st.User.Exists)
	st.Docker = userUnitStatus(ctx, run, cfg.User, "docker.service", st.User.Exists)
	st.Socket = socketStatusOf(cfg.SocketPath())
	st.Port = portStatus{
		Port:      cfg.SSHPort,
		Listening: portListening(cfg.SSHPort),
		APIListen: cfg.APIListen,
	}
	if port := vpnListenPort(cfg.VPNListen); port > 0 {
		st.Port.VPNPort = port
		st.Port.VPNListening = udpBound(port)
	}
	st.Swap = swapStatus{Backend: cfg.Swap.Backend, SizeBytes: cfg.Swap.SizeBytes}
	st.Swap.TotalBytes, st.Swap.Managed = machine.ReadSwap(ctx, run, cfg.SwapFilePath())
	if _, err := os.Stat(serverconfig.BinaryPath); err == nil {
		st.BinaryFound = true
	}
	return st
}

func userStatusOf(name string) userStatus {
	st := userStatus{Name: name}
	u, err := user.Lookup(name)
	if err != nil {
		return st
	}
	st.Exists, st.Home = true, u.HomeDir
	st.UID, _ = strconv.Atoi(u.Uid)
	return st
}

func userUnitStatus(ctx context.Context, run runner.Runner, asUser, unit string, userExists bool) unitStatus {
	st := unitStatus{Name: unit, State: "unknown"}
	if !userExists {
		st.Error = "user " + asUser + " does not exist"
		return st
	}
	active, err := run.Run(ctx, runner.Cmd{Name: "systemctl", Args: []string{"--user", "is-active", unit}, User: asUser})
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.State = strings.TrimSpace(active.Stdout)
	st.Active = active.ExitCode == 0
	enabled, err := run.Run(ctx, runner.Cmd{Name: "systemctl", Args: []string{"--user", "is-enabled", unit}, User: asUser})
	if err == nil {
		st.Enabled = enabled.ExitCode == 0
	}
	return st
}

func socketStatusOf(path string) socketStatus {
	st := socketStatus{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return st
	}
	st.Present = info.Mode()&os.ModeSocket != 0
	st.Mode = fmt.Sprintf("%04o", info.Mode().Perm())
	return st
}

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func vpnListenPort(listen string) int {
	_, p, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return port
}

func udpBound(port int) bool {
	conn, err := net.ListenPacket("udp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

func writeHubStatus(w io.Writer, st hubStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if !st.Installed {
		fmt.Fprintf(tw, "not set up\tno %s (run: sudo caramelo hub setup)\n", st.ConfigFile)
	}
	fmt.Fprintf(tw, "machine\t%s\n", st.Hostname)
	fmt.Fprintf(tw, "version\t%s\n", st.Version)
	fmt.Fprintf(tw, "config\t%s\n", st.ConfigFile)
	fmt.Fprintf(tw, "state\t%s\n", st.Paths.State)
	fmt.Fprintf(tw, "data\t%s\n", st.Paths.Data)
	fmt.Fprintf(tw, "swap\t%s\n", describeSwapStatus(st.Swap))
	fmt.Fprintf(tw, "user\t%s\n", describeUserStatus(st.User))
	fmt.Fprintf(tw, "caramelod\t%s\n", describeUnit(st.Caramelod))
	fmt.Fprintf(tw, "docker\t%s\n", describeUnit(st.Docker))
	fmt.Fprintf(tw, "socket\t%s\n", describeSocket(st.Socket))
	fmt.Fprintf(tw, "port %d\t%s\n", st.Port.Port, describeAPIPort(st.Port))
	if st.Port.VPNPort > 0 {
		fmt.Fprintf(tw, "udp %d\t%s (the machine's network)\n", st.Port.VPNPort,
			yesNo(st.Port.VPNListening, "listening", "not listening"))
	}
	return tw.Flush()
}

func describeSwapStatus(s swapStatus) string {
	switch {
	case s.TotalBytes > 0 && s.Managed:
		return fmtBytesIEC(s.TotalBytes) + " (caramelo)"
	case s.TotalBytes > 0:
		return fmtBytesIEC(s.TotalBytes)
	case s.Backend == serverconfig.SwapOff:
		return "none (swap: off)"
	case s.SizeBytes > 0:
		return "none (" + fmtBytesIEC(s.SizeBytes) + " configured)"
	default:
		return "none"
	}
}

func describeAPIPort(p portStatus) string {
	switch {
	case p.Listening:
		return "listening"
	case p.APIListen == serverconfig.APIListenVPN:
		return "not listening (api_listen vpn: the API answers inside the tunnel and on the socket)"
	default:
		return "not listening"
	}
}

func describeUserStatus(u userStatus) string {
	if !u.Exists {
		return u.Name + " (missing)"
	}
	return fmt.Sprintf("%s (uid %d, home %s)", u.Name, u.UID, u.Home)
}

func describeUnit(u unitStatus) string {
	s := u.State
	if u.Error != "" {
		return s + " (" + u.Error + ")"
	}
	if u.Enabled {
		s += ", enabled"
	} else {
		s += ", not enabled"
	}
	return s
}

func describeSocket(s socketStatus) string {
	if !s.Present {
		return s.Path + " (missing)"
	}
	return fmt.Sprintf("%s (%s)", s.Path, s.Mode)
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}
