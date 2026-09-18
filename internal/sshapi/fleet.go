package sshapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	progresspkg "github.com/plytz/caramelo/internal/progress"
)

type MachineLookup interface {
	MachineNamed(ctx context.Context, name string) (fleet.Machine, bool)
}

type MachineLookupFunc func(ctx context.Context, name string) (fleet.Machine, bool)

func (f MachineLookupFunc) MachineNamed(ctx context.Context, name string) (fleet.Machine, bool) {
	return f(ctx, name)
}

func (s *Server) machinePeer(ctx context.Context, identity string) string {
	if s.Machines == nil || identity == "" {
		return ""
	}
	if _, ok := s.Machines.MachineNamed(ctx, identity); ok {
		return identity
	}
	return ""
}

func (d *Daemon) forwardEnv(ctx context.Context, app, name string) error {
	if d.resolver() == nil || d.forwarder() == nil {
		return nil
	}
	loc, err := d.resolver().ResolveEnv(ctx, app, name)
	if err != nil {
		return err
	}
	if loc.Local {
		return nil
	}
	return d.forwardTo(ctx, loc, nil)
}

func (d *Daemon) forwardMachine(ctx context.Context, machine string) error {
	if d.resolver() == nil || d.forwarder() == nil || machine == "" {
		return nil
	}
	loc, err := d.resolver().ResolveMachine(ctx, machine)
	if err != nil {
		return err
	}
	if loc.Local {
		return nil
	}
	return d.forwardTo(ctx, loc, nil)
}

func (d *Daemon) forwardTo(ctx context.Context, loc api.Location, args []string) error {
	sess, ok := SessionFrom(ctx)
	if len(args) == 0 {
		args = sess.Args
	}
	if !ok || len(args) == 0 {
		return fmt.Errorf("this command is for machine %q and there is no invocation to forward: "+
			"run it through the API rather than in process", loc.Machine)
	}
	if sess.Stdout == nil {
		return fmt.Errorf("this command is for machine %q and this session has no streams to forward it with", loc.Machine)
	}
	stderr := sess.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	if sess.FromMachine() {
		return fmt.Errorf("machine %q asked this machine for something machine %q holds: "+
			"the directory disagrees with itself, and a command is not forwarded twice", sess.Peer, loc.Machine)
	}

	code, err := d.forwarder().Forward(d.withIdentity(ctx), loc, args, sess.Stdin, sess.Stdout, stderr)
	if err != nil {
		return err
	}
	return api.Forwarded(loc.Machine, code)
}

func (d *Daemon) placeAndForward(ctx context.Context, m *env.Manager, req *env.CreateRequest, progress io.Writer) error {
	if d.resolver() == nil || d.forwarder() == nil {
		return nil
	}

	held, err := d.resolver().ResolveEnv(ctx, req.App, req.Name)
	if err != nil {
		return err
	}
	if !held.Local && held.Machine != "" {
		if req.On != "" && req.On != held.Machine {
			return fmt.Errorf("env %q already exists on machine %s, so --on %s is refused: "+
				"destroy it there first with `caramelo env destroy %s --app %s --yes`",
				req.Name, held.Machine, req.On, req.Name, req.App)
		}
		req.On = held.Machine
		return d.forwardTo(ctx, held, withOn(sessionArgs(ctx), held.Machine))
	}
	dec, err := m.PlaceEnv(ctx, *req, d.placementConfig(ctx, m, req.App))
	if err != nil {
		return err
	}
	if dec.Why != "" && progress != nil {

		_ = progresspkg.Emit(progress, progresspkg.Event{
			App: req.App, Env: req.Name,
			Action: "placement", Status: progresspkg.StatusOK, Detail: dec.Why,
		})
	}
	if dec.Machine == "" || dec.Machine == m.FleetWiringOf().Machine {
		return nil
	}
	loc, err := d.resolver().ResolveMachine(ctx, dec.Machine)
	if err != nil {
		return err
	}
	if loc.Local {
		return nil
	}
	req.On = dec.Machine
	return d.forwardTo(ctx, loc, withOn(sessionArgs(ctx), dec.Machine))
}

func (d *Daemon) placementConfig(ctx context.Context, m *env.Manager, app string) *config.App {
	dir, cleanup, err := m.ConfigDir(ctx, app, "")
	if err != nil {
		return nil
	}
	defer cleanup()
	cfg, err := config.LoadDir(dir)
	if err != nil {
		return nil
	}

	cfg.Name = app
	return cfg
}

func sessionArgs(ctx context.Context) []string {
	sess, _ := SessionFrom(ctx)
	return sess.Args
}

func withOn(args []string, machine string) []string {
	for _, a := range args {
		if a == "--on" || strings.HasPrefix(a, "--on=") {
			return args
		}
	}
	return append(append([]string(nil), args...), "--on", machine)
}

func (d *Daemon) requireMachinePeer(ctx context.Context, what, instead string) (fleet.Machine, error) {
	sess, _ := SessionFrom(ctx)
	if sess.Peer == "" {
		return fleet.Machine{}, fmt.Errorf("%s is a call one machine makes of another, "+
			"and this session is %s: run `caramelo %s` instead", what, sessionWho(sess), instead)
	}
	if d.machineLookup() == nil {
		return fleet.Machine{}, fmt.Errorf("%s: this machine keeps no fleet, so it has no members to answer for", what)
	}
	m, ok := d.machineLookup().MachineNamed(ctx, sess.Peer)
	if !ok {
		return fleet.Machine{}, fmt.Errorf("%s: %q is not a machine of this fleet", what, sess.Peer)
	}
	return m, nil
}

func sessionWho(sess api.Session) string {
	if sess.Identity == "" {
		return "on the machine's own socket"
	}
	return "the peer " + sess.Identity + "'s"
}

func (d *Daemon) EnvsMine(ctx context.Context, app string) ([]env.Env, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	sess, _ := SessionFrom(ctx)
	return m.Owned(d.withIdentity(ctx), app, sess.Identity)
}

func (d *Daemon) EnvsAll(ctx context.Context, app string) ([]fleet.DirectoryEntry, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	entries, err := m.Directory(d.withIdentity(ctx), app)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return entries, nil
	}

	local, err := m.List(d.withIdentity(ctx), app)
	if err != nil {
		return nil, err
	}
	out := make([]fleet.DirectoryEntry, 0, len(local))
	for _, e := range local {
		out = append(out, fleet.DirectoryEntry{
			App: e.App, Env: e.Name, Machine: e.Machine, Address: e.VPNIP,
			Owner: e.Owner, Mode: string(e.Mode), Via: e.Via.String(), UpdatedAt: e.UpdatedAt,
		})
	}
	return out, nil
}

func (d *Daemon) EnvHandoff(ctx context.Context, req api.HandoffRequest) (*env.Env, error) {
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	if err := d.forwardEnv(ctx, req.App, req.Name); err != nil {
		return nil, err
	}
	return m.Handoff(d.withIdentity(ctx), req.App, req.Name, req.To)
}

func (d *Daemon) EnvExpose(ctx context.Context, req api.ExposeRequest) (*api.ExposeResult, error) {
	return d.Expose(ctx, req.ExposeRequest)
}

func (d *Daemon) MachineAnnounce(ctx context.Context, a fleet.Announcement) (*api.AnnounceResult, error) {
	peer, err := d.requireMachinePeer(ctx, "an announcement", "member list")
	if err != nil {
		return nil, err
	}

	a.Machine = peer.Name
	m, err := d.envs()
	if err != nil {
		return nil, err
	}
	st := m.FleetWiringOf().Fleet
	if st == nil {
		return nil, errors.New("an announcement: this machine keeps no directory, so it is not a hub")
	}
	accepted, removed, err := env.ApplyAnnouncement(ctx, st, a, d.now())
	if err != nil {
		return nil, err
	}

	if err := m.PushRoutes(ctx); err != nil {
		d.logf("push the route table after %s announced: %v", peer.Name, err)
	}

	if err := d.syncFleetNames(ctx); err != nil {
		d.logf("%v", err)
	}
	d.recordAnnouncement(ctx, peer, a)
	res := &api.AnnounceResult{Machine: peer, Accepted: accepted, Removed: removed}
	if machines, merr := st.Machines(ctx); merr == nil {
		for _, mm := range machines {
			if mm.Role == fleet.RoleHub {
				res.Hub = mm
				break
			}
		}
	}
	return res, nil
}

func (d *Daemon) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
