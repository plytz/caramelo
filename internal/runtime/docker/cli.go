package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
)

type CLI struct {
	Runner runner.Runner

	User string
}

func NewCLI(r runner.Runner, user string) *CLI { return &CLI{Runner: r, User: user} }

var _ runtime.Driver = (*CLI)(nil)

func (c *CLI) Pull(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("docker pull: no image given")
	}
	res, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	_, err = c.runOK(ctx, "pull", image)
	return err
}

func (c *CLI) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}
	args = append(args, labelArgs("--label", labels)...)
	args = append(args, name)
	_, err := c.runOK(ctx, args...)
	return err
}

func (c *CLI) RemoveVolume(ctx context.Context, name string) error {
	res, err := c.run(ctx, "volume", "rm", name)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNoSuchVolume(res) {
		return cmdErr([]string{"volume", "rm", name}, res)
	}
	return nil
}

func (c *CLI) Run(ctx context.Context, spec runtime.ContainerSpec) (string, error) {
	if spec.Name == "" || spec.Image == "" {
		return "", fmt.Errorf("docker run: container needs a name and an image")
	}
	if err := checkImage(spec.Image); err != nil {
		return "", err
	}
	args := append([]string{"run", "--detach"}, runArgs(spec)...)
	res, err := c.runOK(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func runArgs(spec runtime.ContainerSpec) []string {
	var args []string
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.AutoRemove {
		args = append(args, "--rm")
	}
	if spec.TTY {
		args = append(args, "--tty")
	}
	if spec.Restart != "" {
		args = append(args, "--restart", spec.Restart)
	}

	args = append(args, "--log-opt", "max-size="+runtime.LogMaxSize,
		"--log-opt", "max-file="+strconv.Itoa(runtime.LogMaxFiles))

	if v := memoryArg(spec.Memory); v != "" {
		args = append(args, "--memory", v)
	}
	if v := cpuArg(spec.CPU); v != "" {
		args = append(args, "--cpus", v)
	}
	args = append(args, labelArgs("--label", spec.Labels)...)
	for _, p := range spec.Publish {
		args = append(args, "--publish", publishSpec(p))
	}

	if spec.EnvFile != "" {
		args = append(args, "--env-file", spec.EnvFile)
	}
	args = append(args, labelArgs("--env", spec.Env)...)
	for _, v := range spec.Volumes {
		args = append(args, "--volume", volumeSpec(v))
	}
	for _, b := range spec.Binds {
		args = append(args, "--volume", bindSpec(b))
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
		for _, a := range spec.Aliases {
			args = append(args, "--network-alias", a)
		}
	}
	if spec.WorkDir != "" {
		args = append(args, "--workdir", spec.WorkDir)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	args = append(args, spec.Image)
	return append(args, spec.Command...)
}

func memoryArg(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	return strconv.FormatInt(bytes, 10)
}

func cpuArg(cpus float64) string {
	if cpus <= 0 {
		return ""
	}
	return strconv.FormatFloat(cpus, 'f', -1, 64)
}

func checkImage(image string) error {
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("docker run: %q is not an image reference", image)
	}
	return nil
}

func (c *CLI) Inspect(ctx context.Context, name string) (runtime.ContainerState, error) {
	args := []string{"inspect", "--type", "container", "--format", "json", name}
	res, err := c.run(ctx, args...)
	if err != nil {
		return runtime.ContainerState{}, err
	}
	if res.ExitCode != 0 {
		if isNoSuchContainer(res) {
			return runtime.ContainerState{}, fmt.Errorf("container %s: %w", name, runtime.ErrNotFound)
		}
		return runtime.ContainerState{}, cmdErr(args, res)
	}
	found, err := decodeJSON[inspectContainer](res.Stdout)
	if err != nil {
		return runtime.ContainerState{}, fmt.Errorf("parse docker inspect %s: %w", name, err)
	}
	if len(found) == 0 {
		return runtime.ContainerState{}, fmt.Errorf("container %s: %w", name, runtime.ErrNotFound)
	}
	return found[0].state(), nil
}

func (c *CLI) Exec(ctx context.Context, name string, argv []string) (runner.Result, error) {
	if len(argv) == 0 {
		return runner.Result{}, fmt.Errorf("docker exec %s: no command given", name)
	}
	args := append([]string{"exec", name}, argv...)
	return c.run(ctx, args...)
}

func (c *CLI) LogTail(ctx context.Context, name string, tail int) (string, error) {
	n := "all"
	if tail > 0 {
		n = strconv.Itoa(tail)
	}
	args := []string{"logs", "--tail", n, name}
	res, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		if isNoSuchContainer(res) {
			return "", fmt.Errorf("container %s: %w", name, runtime.ErrNotFound)
		}
		return "", cmdErr(args, res)
	}
	out := res.Stdout
	if res.Stderr != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += res.Stderr
	}
	return out, nil
}

func (c *CLI) Remove(ctx context.Context, name string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNoSuchContainer(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) Stop(ctx context.Context, name string, timeout time.Duration) error {
	args := []string{"stop", "--timeout", strconv.Itoa(stopSeconds(timeout)), name}
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNoSuchContainer(res) {
		return cmdErr(args, res)
	}
	return nil
}

func (c *CLI) Restart(ctx context.Context, name string, timeout time.Duration) error {
	args := []string{"restart", "--timeout", strconv.Itoa(stopSeconds(timeout)), name}
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return cmdErr(args, res)
	}
	return nil
}

func stopSeconds(timeout time.Duration) int {
	seconds := int(timeout / time.Second)
	if timeout > 0 && timeout%time.Second != 0 {
		seconds++
	}
	if seconds < 0 {
		seconds = 0
	}
	return seconds
}

func (c *CLI) ListByLabel(ctx context.Context, labels map[string]string) ([]runtime.ContainerState, error) {
	args := []string{"ps", "--all", "--no-trunc"}
	args = append(args, labelArgs("--filter", labels, "label=")...)
	args = append(args, "--format", "json")
	res, err := c.runOK(ctx, args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSON[psRow](res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse docker ps: %w", err)
	}
	out := make([]runtime.ContainerState, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.state())
	}
	return out, nil
}

func (c *CLI) ListVolumesByLabel(ctx context.Context, labels map[string]string) ([]string, error) {
	args := []string{"volume", "ls"}
	args = append(args, labelArgs("--filter", labels, "label=")...)
	args = append(args, "--format", "json")
	res, err := c.runOK(ctx, args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSON[volumeRow](res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse docker volume ls: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out, nil
}

func (c *CLI) run(ctx context.Context, args ...string) (runner.Result, error) {
	if c.Runner == nil {
		return runner.Result{}, fmt.Errorf("docker %s: no command runner configured", strings.Join(args, " "))
	}
	res, err := c.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, User: c.User})
	if err != nil {
		return res, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return res, nil
}

func (c *CLI) runOK(ctx context.Context, args ...string) (runner.Result, error) {
	res, err := c.run(ctx, args...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, cmdErr(args, res)
	}
	return res, nil
}

func cmdErr(args []string, res runner.Result) error {
	shown := shownArgs(args)
	msg := firstLine(res.Stderr)
	if msg == "" {
		msg = firstLine(res.Stdout)
	}
	if msg == "" {
		return fmt.Errorf("docker %s: exit %d", shown, res.ExitCode)
	}
	return fmt.Errorf("docker %s: exit %d: %s", shown, res.ExitCode, msg)
}

func shownArgs(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 1; i < len(out); i++ {
		if out[i-1] != "--env" && out[i-1] != "-e" {
			continue
		}
		if name, _, ok := strings.Cut(out[i], "="); ok {
			out[i] = name + "=***"
		}
	}
	return strings.Join(out, " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func isNoSuchContainer(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object")
}

func isNoSuchVolume(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "no such volume")
}

func labelArgs(flag string, m map[string]string, prefix ...string) []string {
	if len(m) == 0 {
		return nil
	}
	p := ""
	if len(prefix) > 0 {
		p = prefix[0]
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, flag, p+k+"="+m[k])
	}
	return out
}

func publishSpec(p runtime.PortMap) string {
	s := fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort)
	if p.HostIP != "" {
		s = fmt.Sprintf("%s:%s", p.HostIP, s)
	}
	if proto := strings.ToLower(p.Protocol); proto != "" && proto != "tcp" {
		s += "/" + proto
	}
	return s
}

func bindSpec(b runtime.BindMount) string {
	s := b.Host + ":" + b.Path
	if b.ReadOnly {
		s += ":ro"
	}
	return s
}

func volumeSpec(v runtime.VolumeMount) string {
	s := v.Volume + ":" + v.Path
	if v.ReadOnly {
		s += ":ro"
	}
	return s
}

func decodeJSON[T any](s string) ([]T, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "[") {
		var out []T
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var out []T
	for {
		var one T
		err := dec.Decode(&one)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, one)
	}
}

type inspectContainer struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`

	RestartCount int `json:"RestartCount"`
	State        struct {
		Status string `json:"Status"`

		RestartCount int `json:"RestartCount"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]inspectBinding `json:"Ports"`
	} `json:"NetworkSettings"`
	HostConfig struct {
		PortBindings map[string][]inspectBinding `json:"PortBindings"`
	} `json:"HostConfig"`
}

type inspectBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func (c inspectContainer) state() runtime.ContainerState {

	ports := c.NetworkSettings.Ports
	if countBindings(ports) == 0 {
		ports = c.HostConfig.PortBindings
	}
	restarts := c.RestartCount
	if restarts == 0 {

		restarts = c.State.RestartCount
	}
	return runtime.ContainerState{
		Name:     strings.TrimPrefix(c.Name, "/"),
		ID:       c.ID,
		Status:   c.State.Status,
		Image:    c.Config.Image,
		Labels:   c.Config.Labels,
		Ports:    portsFromBindings(ports),
		Restarts: restarts,
	}
}

func countBindings(m map[string][]inspectBinding) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func portsFromBindings(m map[string][]inspectBinding) []runtime.PortMap {
	var out []runtime.PortMap
	for spec, list := range m {
		cport, proto, ok := containerPort(spec)
		if !ok {
			continue
		}
		for _, b := range list {
			hp, err := strconv.Atoi(strings.TrimSpace(b.HostPort))
			if err != nil {
				continue
			}
			out = append(out, runtime.PortMap{
				HostIP:        b.HostIP,
				HostPort:      hp,
				ContainerPort: cport,
				Protocol:      proto,
			})
		}
	}
	sortPorts(out)
	return out
}

type psRow struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Labels string `json:"Labels"`
	Ports  string `json:"Ports"`
}

func (r psRow) state() runtime.ContainerState {
	name := r.Names

	if i := strings.IndexByte(name, ','); i >= 0 {
		name = name[:i]
	}
	return runtime.ContainerState{
		Name:   strings.TrimPrefix(strings.TrimSpace(name), "/"),
		ID:     r.ID,
		Status: r.State,
		Image:  r.Image,
		Labels: parseLabels(r.Labels),
		Ports:  parsePorts(r.Ports),
	}
}

type volumeRow struct {
	Name   string `json:"Name"`
	Labels string `json:"Labels"`
}

func parseLabels(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parsePorts(s string) []runtime.PortMap {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []runtime.PortMap
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		host, spec, mapped := strings.Cut(part, "->")
		if !mapped {
			continue
		}
		cport, proto, ok := containerPort(spec)
		if !ok {
			continue
		}
		i := strings.LastIndex(host, ":")
		if i < 0 {
			continue
		}
		hp, err := strconv.Atoi(strings.TrimSpace(host[i+1:]))
		if err != nil {
			continue
		}
		out = append(out, runtime.PortMap{
			HostIP:        strings.Trim(strings.TrimSpace(host[:i]), "[]"),
			HostPort:      hp,
			ContainerPort: cport,
			Protocol:      proto,
		})
	}
	sortPorts(out)
	return out
}

func containerPort(spec string) (int, string, bool) {
	s := strings.TrimSpace(spec)
	proto := "tcp"
	if i := strings.IndexByte(s, '/'); i >= 0 {
		if p := strings.ToLower(strings.TrimSpace(s[i+1:])); p != "" {
			proto = p
		}
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i > 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, proto, true
}

func sortPorts(p []runtime.PortMap) {
	sort.Slice(p, func(i, j int) bool {
		switch {
		case p[i].ContainerPort != p[j].ContainerPort:
			return p[i].ContainerPort < p[j].ContainerPort
		case p[i].HostPort != p[j].HostPort:
			return p[i].HostPort < p[j].HostPort
		case p[i].HostIP != p[j].HostIP:
			return p[i].HostIP < p[j].HostIP
		default:
			return p[i].Protocol < p[j].Protocol
		}
	})
}
