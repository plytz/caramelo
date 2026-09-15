//go:build integration

package itest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var CommanderTools = []string{"git", "sh", "env", "uname", "cat", "sed"}

var HiddenTools = []string{"ssh", "scp", "sshpass"}

func LookPathIn(pathEnv, name string) (string, bool) {
	if strings.ContainsRune(name, os.PathSeparator) {
		if isExecutable(name) {
			return name, true
		}
		return "", false
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

func NoSSHPath(dir, pathEnv string, tools ...string) (string, error) {
	if len(tools) == 0 {
		tools = CommanderTools
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("no-ssh PATH: %w", err)
	}
	for _, tool := range tools {
		for _, hidden := range HiddenTools {
			if tool == hidden {
				return "", fmt.Errorf("no-ssh PATH: %q is exactly what this PATH must hide", tool)
			}
		}
		target, ok := LookPathIn(pathEnv, tool)
		if !ok {
			return "", fmt.Errorf("no-ssh PATH: %q is not on PATH %q", tool, pathEnv)
		}
		link := filepath.Join(dir, tool)
		if got, err := os.Readlink(link); err == nil && got == target {
			continue
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("no-ssh PATH: %w", err)
		}
		if err := os.Symlink(target, link); err != nil {
			return "", fmt.Errorf("no-ssh PATH: link %s: %w", tool, err)
		}
	}
	for _, hidden := range HiddenTools {
		if p, ok := LookPathIn(dir, hidden); ok {
			return "", fmt.Errorf("no-ssh PATH: %s is still reachable at %s", hidden, p)
		}
	}
	return dir, nil
}

func EnvWithPath(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+path)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+path)
	}
	return out
}

func PathOf(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "PATH="); ok {
			return v
		}
	}
	return ""
}
