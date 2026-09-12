package setup

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const (
	subIDCount = 65536
	subIDStart = 200000
	subIDFloor = 100000
)

const journalGroup = "systemd-journal"

var sessionTimeout = 10 * time.Second

type UserStep struct{}

func NewUserStep() *UserStep { return &UserStep{} }

func (s *UserStep) Name() string { return "user" }

func (s *UserStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	st, err := s.inspect(ctx, env)
	if err != nil {
		return false, "", err
	}
	if len(st.missing) > 0 {
		return false, strings.Join(st.missing, ", "), nil
	}
	return true, fmt.Sprintf("%s (uid %d), subid %d, linger on", env.Config.User, st.uid, st.subStart), nil
}

func (s *UserStep) Apply(ctx context.Context, env *Env) error {
	cfg := env.Config
	st, err := s.inspect(ctx, env)
	if err != nil {
		return err
	}

	if !st.groupExists {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "groupadd", Args: []string{"--system", cfg.Group}}); err != nil {
			return fmt.Errorf("create group %s: %w", cfg.Group, err)
		}
	}
	switch {
	case !st.userExists:
		args := []string{"--system", "--gid", cfg.Group, "--home-dir", cfg.StateDir, "--shell", "/bin/sh"}

		if st.homeExists {
			args = append(args, cfg.User)
			if _, err := mustRun(ctx, env, runner.Cmd{Name: "useradd", Args: args}); err != nil {
				return fmt.Errorf("create user %s: %w", cfg.User, err)
			}
			if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{cfg.User + ":" + cfg.Group, "--", cfg.StateDir}}); err != nil {
				return err
			}
		} else {
			args = append(args, "--create-home", cfg.User)
			if _, err := mustRun(ctx, env, runner.Cmd{Name: "useradd", Args: args}); err != nil {
				return fmt.Errorf("create user %s: %w", cfg.User, err)
			}
		}
	case st.home != cfg.StateDir:

		logf(env, "user %s: home %s -> %s", cfg.User, st.home, cfg.StateDir)
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "usermod", Args: []string{"--home", cfg.StateDir, cfg.User}}); err != nil {
			return fmt.Errorf("set home of %s: %w", cfg.User, err)
		}
	}

	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if err := ensureSubID(ctx, env, f, cfg.User, st.subStart); err != nil {
			return err
		}
	}

	if st.journalGroupExists && !st.inJournalGroup {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "usermod", Args: []string{"-aG", journalGroup, cfg.User}}); err != nil {
			return fmt.Errorf("add %s to %s: %w", cfg.User, journalGroup, err)
		}
	}

	if st.sudoUser != "" && !st.sudoUserInGroup {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "usermod", Args: []string{"-aG", cfg.Group, st.sudoUser}}); err != nil {
			return fmt.Errorf("add %s to group %s: %w", st.sudoUser, cfg.Group, err)
		}
		logf(env, "%s added to group %s (re-login to use the local socket)", st.sudoUser, cfg.Group)
	}

	if !st.linger {
		if _, err := mustRun(ctx, env, runner.Cmd{Name: "loginctl", Args: []string{"enable-linger", cfg.User}}); err != nil {
			return fmt.Errorf("enable linger for %s: %w", cfg.User, err)
		}
	}

	uid := st.uid
	if uid == 0 {
		if uid, err = uidOf(ctx, env, cfg.User); err != nil {
			return err
		}
	}
	if err := waitPath(ctx, env, fmt.Sprintf("/run/user/%d", uid), sessionTimeout); err != nil {
		return fmt.Errorf("systemd session of %s did not start: %w", cfg.User, err)
	}
	return nil
}

type userState struct {
	groupExists        bool
	userExists         bool
	homeExists         bool
	home               string
	uid                int
	subStart           int64
	subUIDOK           bool
	subGIDOK           bool
	journalGroupExists bool
	inJournalGroup     bool

	sudoUser        string
	sudoUserInGroup bool
	linger          bool
	sessionUp       bool
	missing         []string
}

func (s *UserStep) inspect(ctx context.Context, env *Env) (userState, error) {
	cfg := env.Config
	var st userState

	group, err := runCmd(ctx, env, runner.Cmd{Name: "getent", Args: []string{"group", cfg.Group}})
	if err != nil {
		return st, err
	}
	st.groupExists = group.ExitCode == 0
	if !st.groupExists {
		st.missing = append(st.missing, "group "+cfg.Group)
	}

	passwd, err := runCmd(ctx, env, runner.Cmd{Name: "getent", Args: []string{"passwd", cfg.User}})
	if err != nil {
		return st, err
	}
	if passwd.ExitCode == 0 {
		st.userExists = true
		if fields := strings.Split(strings.TrimSpace(passwd.Stdout), ":"); len(fields) >= 6 {
			st.uid, _ = strconv.Atoi(fields[2])
			st.home = fields[5]
		}
		if st.home != cfg.StateDir {
			st.missing = append(st.missing, fmt.Sprintf("home of %s is %s, want %s", cfg.User, st.home, cfg.StateDir))
		}
	} else {
		st.missing = append(st.missing, "user "+cfg.User)
	}

	if home, err := statPath(ctx, env, cfg.StateDir); err != nil {
		return st, err
	} else {
		st.homeExists = home.Exists
	}

	ranges, err := readIDRanges(ctx, env, "/etc/subuid")
	if err != nil {
		return st, err
	}
	gidRanges, err := readIDRanges(ctx, env, "/etc/subgid")
	if err != nil {
		return st, err
	}
	st.subUIDOK = hasRange(ranges, cfg.User)
	st.subGIDOK = hasRange(gidRanges, cfg.User)
	st.subStart = pickSubIDStart(append(append([]idRange{}, ranges...), gidRanges...), cfg.User)
	if !st.subUIDOK || !st.subGIDOK {
		st.missing = append(st.missing, "subuid/subgid range for "+cfg.User)
	}

	jg, err := runCmd(ctx, env, runner.Cmd{Name: "getent", Args: []string{"group", journalGroup}})
	if err != nil {
		return st, err
	}
	st.journalGroupExists = jg.ExitCode == 0
	if st.userExists && st.journalGroupExists {
		groups, err := runCmd(ctx, env, runner.Cmd{Name: "id", Args: []string{"-nG", cfg.User}})
		if err != nil {
			return st, err
		}
		st.inJournalGroup = groups.ExitCode == 0 && contains(strings.Fields(groups.Stdout), journalGroup)
		if !st.inJournalGroup {
			st.missing = append(st.missing, "membership of "+journalGroup)
		}
	}

	st.sudoUser = invokingUser(cfg)
	if st.sudoUser != "" && st.groupExists {
		groups, err := runCmd(ctx, env, runner.Cmd{Name: "id", Args: []string{"-nG", st.sudoUser}})
		if err != nil {
			return st, err
		}
		st.sudoUserInGroup = groups.ExitCode == 0 && contains(strings.Fields(groups.Stdout), cfg.Group)
		if !st.sudoUserInGroup {
			st.missing = append(st.missing, st.sudoUser+" in group "+cfg.Group)
		}
	}

	st.linger, err = succeeds(ctx, env, runner.Cmd{Name: "test", Args: []string{"-e", "/var/lib/systemd/linger/" + cfg.User}})
	if err != nil {
		return st, err
	}
	if !st.linger {
		st.missing = append(st.missing, "linger for "+cfg.User)
	}

	if st.userExists && st.uid > 0 {
		st.sessionUp, err = succeeds(ctx, env, runner.Cmd{Name: "test", Args: []string{"-d", fmt.Sprintf("/run/user/%d", st.uid)}})
		if err != nil {
			return st, err
		}
		if !st.sessionUp {
			st.missing = append(st.missing, "systemd session of "+cfg.User)
		}
	}
	return st, nil
}

type idRange struct {
	Name  string
	Start int64
	Count int64
}

func readIDRanges(ctx context.Context, env *Env, path string) ([]idRange, error) {
	content, _, err := readFile(ctx, env, path)
	if err != nil {
		return nil, err
	}
	return parseIDRanges(content), nil
}

func parseIDRanges(content string) []idRange {
	var out []idRange
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 {
			continue
		}
		start, err1 := strconv.ParseInt(fields[1], 10, 64)
		count, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, idRange{Name: fields[0], Start: start, Count: count})
	}
	return out
}

func hasRange(ranges []idRange, name string) bool {
	for _, r := range ranges {
		if r.Name == name {
			return true
		}
	}
	return false
}

func pickSubIDStart(ranges []idRange, name string) int64 {
	for _, r := range ranges {
		if r.Name == name {
			return r.Start
		}
	}
	for start := int64(subIDStart); start < int64(1)<<32; start += subIDCount {
		if start < subIDFloor {
			continue
		}
		free := true
		for _, r := range ranges {
			if start < r.Start+r.Count && r.Start < start+subIDCount {
				free = false
				break
			}
		}
		if free {
			return start
		}
	}
	return subIDStart
}

func ensureSubID(ctx context.Context, env *Env, path, user string, start int64) error {
	content, exists, err := readFile(ctx, env, path)
	if err != nil {
		return err
	}
	if hasRange(parseIDRanges(content), user) {
		return nil
	}
	line := fmt.Sprintf("%s:%d:%d\n", user, start, subIDCount)
	if exists && content != "" && !strings.HasSuffix(content, "\n") {
		line = "\n" + line
	}
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: "tee", Args: []string{"-a", "--", path}, Stdin: strings.NewReader(line),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("append to %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

var sudoUserEnv = func() string { return os.Getenv("SUDO_USER") }

func invokingUser(cfg serverconfig.Config) string {
	u := strings.TrimSpace(sudoUserEnv())
	if u == "" || u == "root" || u == cfg.User {
		return ""
	}
	return u
}
