package remote

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/serverconfig"
)

type Target struct {
	User string
	Host string
	Port int
}

func (t Target) String() string {
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s@%s:%d", t.User, host, t.Port)
}

func (t Target) Destination() string { return t.User + "@" + t.Host }

func ParseTarget(s string) (Target, error) {
	return ParseTargetWith(s, serverconfig.DefaultUser, serverconfig.DefaultSSHPort)
}

func ParseTargetWith(s, defaultUser string, defaultPort int) (Target, error) {
	t := Target{User: defaultUser, Port: defaultPort}
	rest := strings.TrimSpace(s)
	if rest == "" {
		return Target{}, fmt.Errorf("empty machine address")
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		user, hostpart := rest[:i], rest[i+1:]
		if user == "" {
			return Target{}, fmt.Errorf("machine %q: empty user before @", s)
		}
		t.User, rest = user, hostpart
	}
	host, port, err := splitHostPort(rest)
	if err != nil {
		return Target{}, fmt.Errorf("machine %q: %w", s, err)
	}
	t.Host = host
	if port != 0 {
		t.Port = port
	}
	return t, nil
}

func splitHostPort(s string) (host string, port int, err error) {
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("unclosed [ in address")
		}
		host = s[1:end]
		switch tail := s[end+1:]; {
		case tail == "":
		case strings.HasPrefix(tail, ":"):
			if port, err = parsePort(tail[1:]); err != nil {
				return "", 0, err
			}
		default:
			return "", 0, fmt.Errorf("unexpected %q after ]", tail)
		}
	case strings.Count(s, ":") == 1:
		i := strings.Index(s, ":")
		host = s[:i]
		if port, err = parsePort(s[i+1:]); err != nil {
			return "", 0, err
		}
	default:
		host = s
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty host")
	}
	if strings.ContainsAny(host, " \t") {
		return "", 0, fmt.Errorf("host contains whitespace")
	}
	return host, port, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad port %q", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range", n)
	}
	return n, nil
}
