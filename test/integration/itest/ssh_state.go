//go:build integration && e2e

package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/setup"
	"github.com/plytz/caramelo/test/e2e/inventory"
	"github.com/plytz/caramelo/test/e2e/sshrun"
)

func unreachable(err error) bool { return errors.Is(err, sshrun.ErrUnreachable) }

func (d *sshDriver) ensure(ctx context.Context, state string) error {
	if d.onBox != "" && d.onBox == state {
		d.m.State = state
		d.m.lab.logf("[%s] machine %s is already %s, so this suite reuses it as it stands",
			d.m.Alias, d.entry.Name, state)
		return nil
	}
	d.onBox = ""
	if err := d.ensureClean(ctx); err != nil {
		return err
	}
	d.onBox = StateClean
	d.m.State = state
	if state == StateProvisioned {
		if err := d.provision(ctx); err != nil {
			return err
		}
		d.onBox = StateProvisioned
	}
	return nil
}

func (d *sshDriver) reset(ctx context.Context, state string) error {
	d.onBox = ""
	if err := d.putClean(ctx); err != nil {
		return err
	}
	d.onBox = StateClean
	d.m.State = state
	if state == StateProvisioned {
		if err := d.provision(ctx); err != nil {
			return err
		}
		d.onBox = StateProvisioned
	}
	return nil
}

func (d *sshDriver) ensureClean(ctx context.Context) error {
	failures, err := d.gossFailures(ctx, StateClean+".yaml")
	if err != nil {
		return err
	}
	if len(failures) == 0 {
		return nil
	}
	d.m.lab.logf("[%s] is not clean (%s); putting it back", d.m.Alias, strings.Join(failures, "; "))
	return d.putClean(ctx)
}

func (d *sshDriver) putClean(ctx context.Context) error {
	if len(d.hook) == 0 {
		failures, err := d.gossFailures(ctx, StateClean+".yaml")
		if err != nil {
			return err
		}
		if len(failures) == 0 {
			return nil
		}
		return fmt.Errorf("machine %s is not clean and %s is unset: %s",
			d.entry.Name, ResetEnv, strings.Join(failures, "; "))
	}
	if err := d.runHook(ctx); err != nil {
		return err
	}
	if err := d.reload(ctx); err != nil {
		return err
	}
	failures, err := d.gossFailures(ctx, StateClean+".yaml")
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("machine %s is still not clean after %s: %s",
			d.entry.Name, strings.Join(d.hook, " "), strings.Join(failures, "; "))
	}
	return nil
}

func (d *sshDriver) runHook(ctx context.Context) error {
	argv := append(append([]string(nil), d.hook...), d.entry.Name)
	line := strings.Join(argv, " ")
	d.m.lab.logf("[%s] $ %s", d.m.Alias, line)
	start := time.Now()

	hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.budget().Reset)
	defer cancel()
	cmd := exec.CommandContext(hookCtx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stderr, &out)
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%s (%s): %w: %s", line, ResetEnv, err, strings.TrimSpace(tail(out.String())))
	}
	d.m.lab.logf("[%s] %s finished in %s", d.m.Alias, line, time.Since(start).Round(time.Second))
	return nil
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, "\n")
}

func (d *sshDriver) reload(ctx context.Context) error {
	inv, err := inventory.Load(d.path)
	if err != nil {
		return fmt.Errorf("re-read the inventory after resetting %s: %w", d.entry.Name, err)
	}
	found := false
	for _, entry := range inv.Machines {
		if entry.Name == d.entry.Name {
			d.entry = entry
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%s is no longer in the inventory %s: the reset did not bring it back", d.entry.Name, d.path)
	}
	d.disconnect()
	if err := sshrun.ForgetHostKey(d.dir, d.host()); err != nil {
		return fmt.Errorf("forget the host key of %s: %w", d.entry.Name, err)
	}
	if err := d.waitForBoot(ctx, ""); err != nil {
		return fmt.Errorf("reach %s after the reset: %w", d.entry.Name, err)
	}
	return d.prepare(ctx)
}

func (d *sshDriver) provision(ctx context.Context) error {
	m := d.m
	setupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.budget().Setup)
	defer cancel()

	if err := installLabKeys(setupCtx, m); err != nil {
		return fmt.Errorf("provision %s: %w", m.Alias, err)
	}
	bin, err := BinaryFor(setupCtx, m)
	if err != nil {
		return fmt.Errorf("provision %s: %w", m.Alias, err)
	}
	if err := m.Copy(setupCtx, bin, RemoteBin); err != nil {
		return fmt.Errorf("provision %s: %w", m.Alias, err)
	}
	if err := mustSucceedOn(setupCtx, m, "chmod +x "+RemoteBin); err != nil {
		return fmt.Errorf("provision %s: %w", m.Alias, err)
	}
	kp, err := LabPeerKey()
	if err != nil {
		return fmt.Errorf("provision %s: %w", m.Alias, err)
	}
	cmd := SetupCommandFor(m.Home(), kp.Public)
	m.lab.logf("[%s] fleet setup (this takes minutes)", m.Alias)
	start := time.Now()
	res, err := m.Run(setupCtx, cmd)
	if err != nil {
		return fmt.Errorf("fleet setup on %s: %w", m.Alias, err)
	}
	var report setup.Report
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &report); jsonErr != nil {
		return fmt.Errorf("fleet setup on %s: exit %d, stdout is not a report: %w\nstdout:\n%s\nstderr:\n%s",
			m.Alias, res.ExitCode, jsonErr, res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 || report.Failed != 0 {
		return fmt.Errorf("fleet setup on %s: exit %d, %d step(s) failed\n%s",
			m.Alias, res.ExitCode, report.Failed, res.Stdout)
	}
	m.lab.logf("[%s] fleet setup finished in %s", m.Alias, time.Since(start).Round(time.Second))
	if err := WaitForCaramelod(setupCtx, m); err != nil {
		return err
	}
	return d.relogin(setupCtx)
}

func (d *sshDriver) gossFailures(ctx context.Context, spec string) ([]string, error) {
	path, err := GossSpec(spec)
	if err != nil {
		return nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, d.budget().For(gossTimeout))
	defer cancel()
	if err := ensureGossOn(checkCtx, d.m); err != nil {
		return nil, fmt.Errorf("goss on %s: %w", d.m.Alias, err)
	}
	remote := "/var/tmp/goss-" + filepath.Base(path)
	if err := d.m.Copy(checkCtx, path, remote); err != nil {
		return nil, fmt.Errorf("goss on %s: copy %s: %w", d.m.Alias, path, err)
	}
	vars, err := gossVars(checkCtx, d.m, nil)
	if err != nil {
		return nil, fmt.Errorf("goss on %s: %w", d.m.Alias, err)
	}
	cmd := fmt.Sprintf("sudo -n %s --gossfile %s --vars-inline %s validate --format json",
		GossRemotePath, remote, ShellQuote(vars))
	res, err := d.m.Run(checkCtx, cmd)
	if err != nil {
		return nil, fmt.Errorf("goss on %s: %w", d.m.Alias, err)
	}
	report, err := ParseGossReport([]byte(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("goss %s on %s: %w\nstderr:\n%s", spec, d.m.Alias, err, res.Stderr)
	}
	var out []string
	for _, f := range report.Failures() {
		out = append(out, f.Describe())
	}
	return out, nil
}
