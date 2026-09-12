package sshapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

var (
	ErrKeyNotFound = errors.New("no such key")
	ErrKeyExists   = errors.New("key already exists")
)

type Entry struct {
	Name        string
	Options     string
	Type        string
	Key         string
	Fingerprint string
	Line        string
}

func ListKeys(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseKeys(data), nil
}

func parseKeys(data []byte) []Entry {
	var out []Entry
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, comment, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			continue
		}
		out = append(out, entryFor(key, comment, options))
	}
	return out
}

func entryFor(key gossh.PublicKey, comment string, options []string) Entry {
	e := Entry{
		Name:        strings.TrimSpace(comment),
		Options:     strings.Join(options, ","),
		Type:        key.Type(),
		Key:         base64.StdEncoding.EncodeToString(key.Marshal()),
		Fingerprint: gossh.FingerprintSHA256(key),
	}
	if e.Name == "" {

		e.Name = e.Fingerprint
	}
	e.Line = e.line()
	return e
}

func (e Entry) line() string {
	var b strings.Builder
	if e.Options != "" {
		b.WriteString(e.Options)
		b.WriteByte(' ')
	}
	b.WriteString(e.Type)
	b.WriteByte(' ')
	b.WriteString(e.Key)
	if e.Name != "" {
		b.WriteByte(' ')
		b.WriteString(e.Name)
	}
	return b.String()
}

func (e Entry) PublicKey() (gossh.PublicKey, error) {
	blob, err := base64.StdEncoding.DecodeString(e.Key)
	if err != nil {
		return nil, fmt.Errorf("decode key %s: %w", e.Name, err)
	}
	return gossh.ParsePublicKey(blob)
}

func ParseKey(line, name, options string) (Entry, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Entry{}, errors.New("empty key")
	}
	if strings.ContainsAny(line, "\n\r") {
		return Entry{}, errors.New("key must be a single line")
	}
	key, comment, lineOptions, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return Entry{}, fmt.Errorf("not an OpenSSH public key: %w", err)
	}
	e := entryFor(key, comment, lineOptions)
	if options != "" {
		e.Options = strings.TrimSpace(options)
	}
	if name != "" {
		e.Name = name
	}
	if err := validName(e.Name); err != nil {
		return Entry{}, err
	}
	e.Line = e.line()
	return e, nil
}

func validName(name string) error {
	switch {
	case name == "":
		return errors.New("key name must not be empty")
	case strings.ContainsAny(name, " \t\n\r"):
		return fmt.Errorf("key name %q must not contain whitespace", name)
	}
	return nil
}

func AppendKey(path, line, name string) (Entry, error) {
	return AppendKeyWithOptions(path, line, name, "")
}

func AppendKeyWithOptions(path, line, name, options string) (Entry, error) {
	e, err := ParseKey(line, name, options)
	if err != nil {
		return Entry{}, err
	}
	existing, err := ListKeys(path)
	if err != nil {
		return Entry{}, err
	}
	for _, x := range existing {
		switch {
		case x.Name == e.Name:
			return Entry{}, fmt.Errorf("key %q: %w", e.Name, ErrKeyExists)
		case x.Fingerprint == e.Fingerprint:
			return Entry{}, fmt.Errorf("key %s is already authorized as %q: %w", e.Fingerprint, x.Name, ErrKeyExists)
		}
	}
	if err := writeKeys(path, append(existing, e)); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func RemoveKey(path, name string) (Entry, error) {
	existing, err := ListKeys(path)
	if err != nil {
		return Entry{}, err
	}
	var removed Entry
	kept := make([]Entry, 0, len(existing))
	for _, x := range existing {
		if x.Name == name && removed.Name == "" {
			removed = x
			continue
		}
		kept = append(kept, x)
	}
	if removed.Name == "" {
		return Entry{}, fmt.Errorf("key %q: %w", name, ErrKeyNotFound)
	}
	if err := writeKeys(path, kept); err != nil {
		return Entry{}, err
	}
	return removed, nil
}

func writeKeys(path string, entries []Entry) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	var b strings.Builder
	b.WriteString("# Managed by caramelod. Use 'caramelo key add|remove'.\n")
	for _, e := range entries {
		b.WriteString(e.Line)
		b.WriteByte('\n')
	}
	if err := writeFileAtomic(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

type Authorizer struct {
	Path string

	User string

	Log io.Writer
}

var contextKeyIdentity = &struct{ name string }{"caramelo-identity"}

func (a *Authorizer) Authorize(ctx ssh.Context, key ssh.PublicKey) bool {
	if ctx.User() != a.User {
		a.logf("auth rejected: user %q (only %q is accepted) from %s", ctx.User(), a.User, ctx.RemoteAddr())
		return false
	}
	entries, err := ListKeys(a.Path)
	if err != nil {
		a.logf("auth rejected: %v", err)
		return false
	}
	for _, e := range entries {
		pk, err := e.PublicKey()
		if err != nil {
			continue
		}
		if ssh.KeysEqual(pk, key) {
			ctx.SetValue(contextKeyIdentity, e.Name)
			return true
		}
	}
	a.logf("auth rejected: unauthorized key %s from %s", gossh.FingerprintSHA256(key), ctx.RemoteAddr())
	return false
}

func (a *Authorizer) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	fmt.Fprintf(a.Log, "%s caramelod: "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func identityOf(ctx ssh.Context) string {
	if v, ok := ctx.Value(contextKeyIdentity).(string); ok {
		return v
	}
	return ""
}
