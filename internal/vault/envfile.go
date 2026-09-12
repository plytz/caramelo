package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func EnvFileContent(values map[string]string) (string, error) {
	var b strings.Builder
	for _, name := range sortedKeys(values) {
		v := values[name]
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("the secret %s contains a newline, which Docker's --env-file "+
				"cannot carry: base64 it, or put it in a file the service reads", name)
		}
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

type EnvFile struct {
	Path string

	Names []string
}

func WriteEnvFile(dir, name string, values map[string]string) (*EnvFile, error) {
	if len(values) == 0 {
		return &EnvFile{}, nil
	}
	content, err := EnvFileContent(values)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, SecretsDirMode); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	if err := os.Chmod(dir, SecretsDirMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, envFilePrefix(name))
	if err != nil {
		return nil, fmt.Errorf("write the secrets of %s: %w", name, err)
	}

	if err := f.Chmod(SecretsFileMode); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("write the secrets of %s: %w", name, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("write the secrets of %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("write the secrets of %s: %w", name, err)
	}
	return &EnvFile{Path: f.Name(), Names: sortedKeys(values)}, nil
}

func (f *EnvFile) Remove() error {
	if f == nil || f.Path == "" {
		return nil
	}
	path := f.Path
	err := os.Remove(path)
	f.Path = ""
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func envFilePrefix(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "secrets"
	}
	return out + "-"
}

func ParseDotenv(content string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, err := parseDotenvLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d: %s is set twice", i+1, name)
		}
		if err := ValidateValue(name, value); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out[name] = value
	}
	return out, nil
}

func ReadDotenvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	values, err := ParseDotenv(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Clean(path), err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s holds no KEY=value lines", filepath.Clean(path))
	}
	return values, nil
}

func parseDotenvLine(line string) (name, value string, err error) {
	line = strings.TrimPrefix(line, "export ")
	name, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", fmt.Errorf("%q is not NAME=value", redactLine(line))
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return "", "", err
	}
	value, err = unquote(strings.TrimSpace(value))
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", name, err)
	}
	return name, value, nil
}

func unquote(v string) (string, error) {
	if len(v) < 2 {
		return v, nil
	}
	switch {
	case v[0] == '\'' && v[len(v)-1] == '\'':
		return v[1 : len(v)-1], nil
	case v[0] == '"' && v[len(v)-1] == '"':
		return unescape(v[1 : len(v)-1])
	}
	return v, nil
}

func unescape(v string) (string, error) {
	if !strings.Contains(v, `\`) {
		return v, nil
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i == len(v)-1 {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '\\', '"', '\'':
			b.WriteByte(v[i])
		default:
			return "", fmt.Errorf(`\%s is not an escape (\n \r \t \\ \" \' are)`, string(v[i]))
		}
	}
	return b.String(), nil
}

func redactLine(line string) string {
	const show = 16
	if len(line) <= show {
		return line
	}
	return line[:show] + "…"
}

const (
	FormatEnv  = "env"
	FormatJSON = "json"
)

func Render(values map[string]string, format string) (string, error) {
	switch format {
	case "", FormatEnv:
		var b strings.Builder
		for _, name := range sortedKeys(values) {

			if strings.ContainsAny(values[name], "\n\r") {
				return "", fmt.Errorf("%s has a newline in it, which %s lines cannot carry: "+
					"export with --format %s", name, FormatEnv, FormatJSON)
			}
			b.WriteString(name)
			b.WriteByte('=')
			b.WriteString(shellQuote(values[name]))
			b.WriteByte('\n')
		}
		return b.String(), nil
	case FormatJSON:
		var b strings.Builder
		b.WriteString("{\n")
		names := sortedKeys(values)
		for i, name := range names {
			b.WriteString("  ")
			b.WriteString(strconv.Quote(name))
			b.WriteString(": ")
			b.WriteString(strconv.Quote(values[name]))
			if i < len(names)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("}\n")
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown export format %q: want %s or %s", format, FormatEnv, FormatJSON)
}

func shellQuote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n\r\"'$`\\|&;<>()*?[]#~=%!{}") {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func EnsureSecretsDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.MkdirAll(dir, SecretsDirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, SecretsDirMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	return nil
}
