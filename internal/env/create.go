package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/git"
	"github.com/plytz/caramelo/internal/ports"
	"github.com/plytz/caramelo/internal/runtime"
	"github.com/plytz/caramelo/internal/state"
)

const allocAttempts = 32

const logTail = 20

func (m *Manager) Create(ctx context.Context, req CreateRequest, progress io.Writer) (*Env, error) {
	if err := ValidateName("app", req.App); err != nil {
		return nil, err
	}
	if err := ValidateName("env", req.Name); err != nil {
		return nil, err
	}

	if err := m.acceptPlacement(req); err != nil {
		return nil, err
	}
	defer m.lockEnv(req.App, req.Name)()

	app, err := m.Store.App(ctx, req.App)
	switch {
	case errors.Is(err, state.ErrNotFound):

		if app, err = m.mirrorApp(ctx, req, progress); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("read app %q: %w", req.App, err)
	}

	retry, err := m.clearFailed(ctx, req, progress)
	if err != nil {
		return nil, err
	}

	if err := m.SetSecretsBefore(ctx, req.App, req.Name, req.Secrets, progress); err != nil {
		return nil, err
	}

	repo := RepoPath(m.Dirs.Data, req.App)

	plan, err := m.planBranch(ctx, app, repo, req, retry)
	if m.fw().Mirror != nil {
		if _, merr := m.mirrorApp(ctx, req, progress); merr != nil {
			if err != nil {
				return nil, merr
			}
			progressf(progress, "warning", "mirror", "%v; building %s from what this machine already has",
				merr, req.App)
		} else {
			plan, err = m.planBranch(ctx, app, repo, req, retry)
		}
	}
	if err != nil {
		return nil, err
	}

	worktree := WorktreePath(m.Dirs.Data, req.App, req.Name)
	rec, err := m.createRow(ctx, req, plan, worktree)
	if err != nil {
		return nil, err
	}
	m.event(ctx, rec.ID, "create", "started", fmt.Sprintf("branch %s at %s", plan.branch, short(plan.commit)))

	progress = m.feed(ctx, rec, progress)
	progressf(progress, "ok", "ports", "%d-%d", rec.PortBase, rec.PortBase+rec.PortCount-1)

	if err := m.buildTree(ctx, rec, repo, plan, progress); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	cfg, err := m.readConfig(worktree, req.App, progress)
	if err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	if err := m.setFleetRow(ctx, rec.ID, FleetRow{
		Owner: IdentityFrom(ctx),
		Via:   cfg.ViaOf(rec.Name),
	}); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	secrets, err := m.ensureDepPasswords(ctx, rec, cfg.Deps, progress)
	if err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}
	resolved, vars, err := m.resolve(cfg, rec, secrets)
	if err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	if err := m.saveEnv(ctx, rec, cfg, vars, state.EnvCreating); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	network, err := m.ensureEnvNetwork(ctx, rec, progress)
	if err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}
	if _, err := m.ensureCacheVolume(ctx, rec, ""); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	if req.NoDeps {
		progressf(progress, "skipped", "deps", "--no-deps")
	} else if err := m.startDeps(ctx, rec, resolved, req, secrets, progress); err != nil {
		return nil, err
	} else if err := m.attachDeps(ctx, rec, resolved.Deps, network, progress); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}

	m.publish(ctx, rec, cfg, progress)

	if err := m.saveEnv(ctx, rec, cfg, vars, state.EnvReady); err != nil {
		return nil, m.fail(ctx, rec, progress, "", err)
	}
	m.event(ctx, rec.ID, "create", "ok", fmt.Sprintf("ports %d-%d", rec.PortBase, rec.PortBase+rec.PortCount-1))
	progressf(progress, "changed", "env", "%s ready on port %d", rec.Name, rec.PortBase)

	m.announce(ctx, rec)
	e, err := recordToEnv(rec)
	if err != nil {
		return nil, err
	}
	m.stampFleet(e, m.fleetRow(ctx, rec.ID))
	return e, nil
}

func (m *Manager) clearFailed(ctx context.Context, req CreateRequest, progress io.Writer) (bool, error) {
	existing, err := m.Store.Env(ctx, req.App, req.Name)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read env %q: %w", req.Name, err)
	case existing.Status != state.EnvFailed:

		return false, fmt.Errorf("env %q already exists (status %s): destroy it first with "+
			"'caramelo env destroy %s --yes'", req.Name, existing.Status, req.Name)
	}
	progressf(progress, "changed", "retry", "removing the failed env %q", req.Name)
	if err := m.destroyLocked(ctx, req.App, req.Name, false, progress); err != nil {
		return false, fmt.Errorf("clean up the failed env %q before retrying: %w", req.Name, err)
	}
	return true, nil
}

type branchPlan struct {
	branch string
	from   string
	commit string

	create bool

	reset bool
}

func (m *Manager) planBranch(ctx context.Context, app *state.App, repo string, req CreateRequest, retry bool) (branchPlan, error) {
	p := branchPlan{branch: req.Name, from: req.From}
	exists, err := m.Git.BranchExists(ctx, repo, p.branch)
	if err != nil {
		return p, fmt.Errorf("look for branch %q: %w", p.branch, err)
	}
	switch {
	case exists && req.Reset && p.from == "":
		return p, fmt.Errorf("--reset needs --from: there is nothing to move branch %q to", p.branch)
	case exists && (req.Reset || (retry && p.from != "")):
		p.create, p.reset = true, true
	case exists && p.from != "":
		return p, fmt.Errorf("branch %q already exists: omit --from to build on it, or pass --reset to move it to %q", p.branch, p.from)
	case exists:
	default:
		p.create = true
		if p.from == "" {
			if p.from, err = m.defaultBranch(ctx, app, repo); err != nil {
				return p, err
			}
		}
	}

	ref := p.from
	if !p.create {
		ref = p.branch
	}
	p.commit, err = m.resolveRef(ctx, repo, ref)
	switch {
	case errors.Is(err, git.ErrNotFound):
		return p, fmt.Errorf("no such ref %q in app %q", ref, req.App)
	case err != nil:
		return p, fmt.Errorf("resolve %q: %w", ref, err)
	}
	return p, nil
}

func (m *Manager) resolveRef(ctx context.Context, repo, ref string) (string, error) {
	commit, err := m.Git.RevParse(ctx, repo, ref)
	if !errors.Is(err, git.ErrNotFound) || m.fw().Mirror == nil || strings.HasPrefix(ref, git.MirrorRemote+"/") {
		return commit, err
	}
	return m.Git.RevParse(ctx, repo, git.MirrorRemote+"/"+ref)
}

func (m *Manager) defaultBranch(ctx context.Context, app *state.App, repo string) (string, error) {
	if app != nil && app.DefaultBranch != "" {
		return app.DefaultBranch, nil
	}
	head, err := m.Git.SymbolicRefHEAD(ctx, repo)
	switch {
	case errors.Is(err, git.ErrNotFound):
		return "", fmt.Errorf("%s has no default branch yet: pass --from", repo)
	case err != nil:
		return "", fmt.Errorf("read HEAD of %s: %w", repo, err)
	}
	return trimRefsHeads(head), nil
}

func trimRefsHeads(ref string) string {
	const prefix = "refs/heads/"
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):]
	}
	return ref
}

func (m *Manager) createRow(ctx context.Context, req CreateRequest, plan branchPlan, worktree string) (*state.EnvRecord, error) {
	now := m.now()
	for attempt := 0; attempt < allocAttempts; attempt++ {
		base, err := m.Ports.Allocate(ctx, 0)
		if err != nil {
			return nil, fmt.Errorf("allocate a port block: %w", err)
		}

		ip, err := m.allocAddress(ctx)
		if err != nil {
			return nil, err
		}
		rec, err := m.Store.CreateEnv(ctx, state.EnvRecord{
			App:       req.App,
			Name:      req.Name,
			Branch:    plan.branch,
			Commit:    plan.commit,
			Worktree:  worktree,
			PortBase:  base,
			PortCount: ports.BlockSize,
			VPNIP:     addrString(ip),
			Status:    state.EnvCreating,

			Mode:      string(req.Mode()),
			Protected: req.IsProtected(),
			CreatedBy: IdentityFrom(ctx),
			CreatedAt: now,
			UpdatedAt: now,
		})
		switch {
		case err == nil:
			return rec, nil
		case errors.Is(err, state.ErrExists):

			if _, e := m.Store.Env(ctx, req.App, req.Name); e == nil {
				return nil, fmt.Errorf("env %q already exists", req.Name)
			}
			continue
		default:
			return nil, fmt.Errorf("record env %q: %w", req.Name, err)
		}
	}
	return nil, fmt.Errorf("record env %q: %d port blocks in a row were taken by another create", req.Name, allocAttempts)
}

func (m *Manager) buildTree(ctx context.Context, rec *state.EnvRecord, repo string, plan branchPlan, progress io.Writer) error {
	if plan.create {
		if plan.reset {
			if err := m.Git.DeleteBranch(ctx, repo, plan.branch, true); err != nil {
				return fmt.Errorf("reset branch %q: %w", plan.branch, err)
			}
		}

		from := plan.from
		if plan.commit != "" {
			from = plan.commit
		}
		if err := m.Git.CreateBranch(ctx, repo, plan.branch, from); err != nil {
			return fmt.Errorf("create branch %q from %q: %w", plan.branch, plan.from, err)
		}
		m.addResource(ctx, rec.ID, state.ResourceBranch, plan.branch, "", 0)
		progressf(progress, "changed", "branch", "%s from %s", plan.branch, plan.from)
	} else {
		progressf(progress, "ok", "branch", "%s at %s", plan.branch, short(plan.commit))
	}

	if err := m.Git.WorktreeAdd(ctx, repo, rec.Worktree, plan.branch); err != nil {
		return fmt.Errorf("add worktree %s: %w", rec.Worktree, err)
	}
	m.addResource(ctx, rec.ID, state.ResourceWorktree, rec.Worktree, "", 0)
	progressf(progress, "changed", "worktree", "%s", rec.Worktree)
	return nil
}

func (m *Manager) readConfig(worktree, app string, progress io.Writer) (*config.App, error) {
	path := filepath.Join(worktree, config.FileName)
	cfg, err := m.loadConfig(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		progressf(progress, "skipped", "config", "no %s: worktree and PORT only", config.FileName)
		return &config.App{Name: app}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Name == "" {
		cfg.Name = app
	}
	progressf(progress, "ok", "config", "%s (%s)", config.FileName, plural(len(cfg.Deps), "dep"))
	return cfg, nil
}

func (m *Manager) resolve(cfg *config.App, rec *state.EnvRecord,
	secrets *envSecrets) (*config.App, map[string]string, error) {
	l, err := m.layoutOf(rec, cfg)
	if err != nil {
		return nil, nil, err
	}
	ectx := l.views(rec, cfg.Deps).ContextWith(config.ViewHost, secrets.redacted())

	out := *cfg
	expanded, err := m.expand(cfg.Env, ectx)
	if err != nil {
		return nil, nil, fmt.Errorf("expand the variables of env %q: %w", rec.Name, err)
	}
	out.Env = expanded
	out.Deps = make([]config.Dep, len(cfg.Deps))
	for i, d := range cfg.Deps {
		if d.Env, err = m.expand(d.Env, ectx); err != nil {
			return nil, nil, fmt.Errorf("expand the variables of dependency %q: %w", cfg.Deps[i].Name, err)
		}
		out.Deps[i] = d
	}
	return &out, managedVars(rec, expanded, ""), nil
}

func (m *Manager) saveEnv(ctx context.Context, rec *state.EnvRecord, cfg *config.App, vars map[string]string, status string) error {
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		return fmt.Errorf("encode variables: %w", err)
	}
	rec.ConfigJSON, rec.VarsJSON, rec.Status, rec.UpdatedAt = string(cfgJSON), string(varsJSON), status, m.now()
	if err := m.Store.UpdateEnv(ctx, *rec); err != nil {
		return fmt.Errorf("record env %q: %w", rec.Name, err)
	}
	return nil
}

func (m *Manager) startDeps(ctx context.Context, rec *state.EnvRecord, cfg *config.App, req CreateRequest,
	secrets *envSecrets, progress io.Writer) error {
	for i, dep := range cfg.Deps {
		container := ContainerName(rec.App, rec.Name, dep.Name)
		if err := m.startDep(ctx, rec, dep, depPort(rec.PortBase, i), secrets, progress); err != nil {
			return m.fail(ctx, rec, progress, container, err)
		}
		if err := m.waitReady(ctx, dep, container, depPort(rec.PortBase, i), m.timeout(req.Timeout), progress); err != nil {
			return m.fail(ctx, rec, progress, container, err)
		}
	}
	return nil
}

func (m *Manager) startDep(ctx context.Context, rec *state.EnvRecord, dep config.Dep, port int,
	secrets *envSecrets, progress io.Writer) error {
	labels := Labels(rec.App, rec.Name, dep.Name, m.Version)
	if err := m.Driver.Pull(ctx, dep.Image); err != nil {
		return fmt.Errorf("pull %s: %w", dep.Image, err)
	}
	progressf(progress, "ok", "image", "%s", dep.Image)

	spec := runtime.ContainerSpec{
		Name:    ContainerName(rec.App, rec.Name, dep.Name),
		Image:   dep.Image,
		Env:     depEnv(dep, secrets),
		Labels:  labels,
		Publish: []runtime.PortMap{{HostIP: DepHost, HostPort: port, ContainerPort: dep.Port}},
		Restart: runtime.RestartUnlessStopped,
	}
	var depSecret map[string]string
	if depPasswordIsSecret(dep, secrets) {
		known, _ := config.Lookup(dep.Image)
		depSecret = map[string]string{known.PasswordVar: spec.Env[known.PasswordVar]}
		delete(spec.Env, known.PasswordVar)
	}
	if dep.Data != "" {
		volume := VolumeName(rec.App, rec.Name, dep.Name)
		if err := m.Driver.CreateVolume(ctx, volume, labels); err != nil {
			return fmt.Errorf("create volume %s: %w", volume, err)
		}
		m.addResource(ctx, rec.ID, state.ResourceVolume, volume, dep.Name, 0)
		progressf(progress, "changed", "volume", "%s", volume)
		spec.Volumes = []runtime.VolumeMount{{Volume: volume, Path: dep.Data}}
	}

	if _, err := m.runContainer(ctx, spec, depSecret); err != nil {
		return fmt.Errorf("start %s: %w", spec.Name, err)
	}
	m.addResource(ctx, rec.ID, state.ResourceContainer, spec.Name, dep.Name, port)
	progressf(progress, "changed", "container", "%s on %s:%d", spec.Name, DepHost, port)
	return nil
}

func (m *Manager) waitReady(ctx context.Context, dep config.Dep, container string, port int, timeout time.Duration, progress io.Writer) error {
	if len(dep.Ready) == 0 {
		progressf(progress, "warning", "ready", "%s has no readiness command: falling back to a TCP connect on %s:%d, which only proves the port is published", dep.Name, DepHost, port)
	}
	deadline := m.now().Add(timeout)
	var last string
	for {
		ok, detail := m.readyOnce(ctx, dep, container, port)
		if ok {
			progressf(progress, "ok", "ready", "%s", dep.Name)
			return nil
		}
		if detail != "" {
			last = detail
		}
		if !m.now().Before(deadline) {
			break
		}
		if err := sleep(ctx, m.readyInterval()); err != nil {
			return err
		}
	}
	err := fmt.Errorf("dependency %q was not ready after %s", dep.Name, timeout)
	if last != "" {
		err = fmt.Errorf("%w: %s", err, last)
	}
	return err
}

func (m *Manager) readyOnce(ctx context.Context, dep config.Dep, container string, port int) (bool, string) {
	if len(dep.Ready) == 0 {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(DepHost, strconv.Itoa(port)), m.readyInterval())
		if err != nil {
			return false, err.Error()
		}
		conn.Close()
		return true, ""
	}
	res, err := m.Driver.Exec(ctx, container, dep.Ready)
	switch {
	case err != nil:
		return false, err.Error()
	case res.ExitCode == 0:
		return true, ""
	}
	detail := firstLine(res.Stderr)
	if detail == "" {
		detail = firstLine(res.Stdout)
	}
	return false, fmt.Sprintf("exit %d: %s", res.ExitCode, detail)
}

func (m *Manager) fail(ctx context.Context, rec *state.EnvRecord, progress io.Writer, container string, cause error) error {

	bg := context.WithoutCancel(ctx)
	if err := m.Store.UpdateEnvStatus(bg, rec.ID, state.EnvFailed); err != nil {
		progressf(progress, "warning", "state", "could not mark env %q failed: %v", rec.Name, err)
	}
	rec.Status = state.EnvFailed
	m.event(bg, rec.ID, "create", "failed", cause.Error())
	progressf(progress, "failed", "env", "%s: %v", rec.Name, cause)
	if container != "" {
		if out, err := m.Driver.LogTail(bg, container, logTail); err == nil && out != "" {
			progressf(progress, "warning", "logs", "last %d lines of %s:\n%s", logTail, container, out)
		}
	}
	return cause
}

func (m *Manager) addResource(ctx context.Context, envID int64, kind, name, dep string, port int) {

	_ = m.Store.AddResource(ctx, state.EnvResource{
		EnvID:     envID,
		Kind:      kind,
		Name:      name,
		Dep:       dep,
		Port:      port,
		CreatedAt: m.now(),
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func addrString(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

func (m *Manager) mirrorApp(ctx context.Context, req CreateRequest, progress io.Writer) (*state.App, error) {
	if m.fw().Mirror == nil {
		return nil, fmt.Errorf("no such app %q: push it to the machine first", req.App)
	}
	progressf(progress, "started", "mirror", "fetching %s from the hub", req.App)
	if err := m.fw().Mirror(ctx, req.App, req.From); err != nil {
		return nil, fmt.Errorf("fetch app %q from the hub: %w", req.App, err)
	}
	app, err := m.Store.App(ctx, req.App)
	if err != nil {
		return nil, fmt.Errorf("read app %q after fetching it from the hub: %w", req.App, err)
	}
	progressf(progress, "changed", "mirror", "%s fetched from the hub", req.App)
	return app, nil
}
