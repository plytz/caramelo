package config

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Expose string

const (
	ExposeNone Expose = "none"

	ExposeHTTPS Expose = "https"

	ExposeTLS            Expose = "tls"
	ExposeTLSPassthrough Expose = "tls-passthrough"
	ExposeTCP            Expose = "tcp"
	ExposeUDP            Expose = "udp"
)

var ReservedExposeKinds = []Expose{ExposeTLS, ExposeTLSPassthrough, ExposeTCP, ExposeUDP}

const DefaultDrain = 30 * time.Second

const DefaultReplicas = 1

const MaxReplicas = 8

const DomainAuto = "auto"

const SslipSuffix = "sslip.io"

func ParseExpose(s string) (Expose, error) {
	v := strings.TrimSpace(s)
	switch Expose(v) {
	case "":
		return "", nil
	case ExposeNone:
		return ExposeNone, nil
	case ExposeHTTPS:
		return ExposeHTTPS, nil
	}

	kind := Expose(v)
	if i := strings.IndexByte(v, ':'); i > 0 {
		kind = Expose(v[:i])
	}
	for _, r := range ReservedExposeKinds {
		if kind == r {
			return "", fmt.Errorf("expose %q is not supported yet: %s and %s are what M6 serves; "+
				"a database is reached over the machine's private network (caramelo vpn up)",
				v, ExposeHTTPS, ExposeNone)
		}
	}
	return "", fmt.Errorf("unknown expose %q: want %s or %s", v, ExposeHTTPS, ExposeNone)
}

func (s Service) Replicas() int {
	if s.ReplicaCount <= 0 {
		return DefaultReplicas
	}
	return s.ReplicaCount
}

func (s Service) DrainTimeout() time.Duration {
	if s.Drain <= 0 {
		return DefaultDrain
	}
	return s.Drain
}

func (a *App) ExposeOf(name string) Expose {
	if a == nil {
		return ExposeNone
	}
	svc, ok := a.Service(name)
	if !ok {
		return ExposeNone
	}
	if svc.Expose != "" {
		return svc.Expose
	}
	if a.Domain == "" || !svc.HasPort() {
		return ExposeNone
	}
	if first, ok := a.firstExposable(); ok && first.Name == name {
		return ExposeHTTPS
	}
	return ExposeNone
}

func (a *App) firstExposable() (Service, bool) {
	for _, s := range a.Services {
		if s.HasPort() && s.Expose != ExposeNone && s.Protocol != ProtocolUDP {
			return s, true
		}
	}
	return Service{}, false
}

func (a *App) ExposedServices() []Service {
	if a == nil {
		return nil
	}
	var out []Service
	for _, s := range a.Services {
		if a.ExposeOf(s.Name) == ExposeHTTPS {
			out = append(out, s)
		}
	}
	return out
}

func (a *App) HostFor(env, service string) string {
	if a == nil || a.Domain == "" || env == "" {
		return ""
	}
	if a.ExposeOf(service) != ExposeHTTPS {
		return ""
	}
	return DerivedHost(a.Domain, env, service, a.isFirstExposed(service))
}

func (a *App) isFirstExposed(service string) bool {
	exposed := a.ExposedServices()
	return len(exposed) > 0 && exposed[0].Name == service
}

func DerivedHost(domain, env, service string, first bool) string {
	domain = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(domain)), ".")
	if domain == "" || env == "" {
		return ""
	}
	if first || service == "" {
		return env + "." + domain
	}
	return service + "." + env + "." + domain
}

func AutoDomain(app string, ip netip.Addr) (string, error) {
	switch {
	case strings.TrimSpace(app) == "":
		return "", fmt.Errorf("domain %q: no app name to build a name from", DomainAuto)
	case !ip.IsValid() || !ip.Is4():
		return "", fmt.Errorf("domain %q: needs the machine's public IPv4 address, got %q", DomainAuto, ip)
	}
	return fmt.Sprintf("%s.%s.%s", app, strings.ReplaceAll(ip.String(), ".", "-"), SslipSuffix), nil
}

func validateDomain(path, domain string) error {
	if domain == "" || domain == DomainAuto {
		return nil
	}
	if err := ValidateHostname(domain); err != nil {
		return keyErr(path, "domain", "%v", err)
	}
	return nil
}

const (
	MaxHostLen = 253

	MaxLabelLen = 63
)

func ValidateHostname(host string) error {
	switch {
	case strings.TrimSpace(host) == "":
		return fmt.Errorf("a hostname is required")
	case strings.Contains(host, "/"):
		return fmt.Errorf("%q is a hostname, not a URL", host)
	case strings.Contains(host, ":"):
		return fmt.Errorf("a hostname must not carry a port: got %q", host)
	case strings.HasPrefix(host, ".") || strings.HasSuffix(host, "."):
		return fmt.Errorf("a hostname must not start or end with %q: got %q", ".", host)
	case strings.Contains(host, ".."):
		return fmt.Errorf("%q has an empty label", host)
	case !strings.Contains(host, "."):
		return fmt.Errorf("%q must be a fully-qualified name (e.g. %q) or %q", host, "shop.example.com", DomainAuto)
	case host != strings.ToLower(host):
		return fmt.Errorf("a hostname must be lowercase: got %q", host)
	case len(host) > MaxHostLen:
		return fmt.Errorf("%q is longer than the %d characters a name may hold", host, MaxHostLen)
	}
	for _, label := range strings.Split(host, ".") {
		switch {
		case strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-"):
			return fmt.Errorf("label %q must not start or end with %q", label, "-")
		case len(label) > MaxLabelLen:
			return fmt.Errorf("label %q is longer than the %d characters a label may hold", label, MaxLabelLen)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("%q is not a hostname: %q is not allowed", host, string(r))
			}
		}
	}
	return nil
}

func (a *App) validateEdge() error {
	if err := validateDomain(a.Path, a.Domain); err != nil {
		return err
	}
	for _, s := range a.Services {
		key := "services." + s.Name
		if s.ReplicaCount != 0 && (s.ReplicaCount < 1 || s.ReplicaCount > MaxReplicas) {
			return keyErr(a.Path, key+".replicas", "%d replicas is out of range (1-%d)", s.ReplicaCount, MaxReplicas)
		}
		if s.Drain < 0 {
			return keyErr(a.Path, key+".drain", "must not be negative, got %s", s.Drain)
		}
		switch s.Expose {
		case "", ExposeNone:
		case ExposeHTTPS:
			switch {
			case !s.HasPort():
				return keyErr(a.Path, key+".expose", "an exposed service needs a port, and this one wrote %q",
					"port: "+PortNoneValue)
			case s.Protocol == ProtocolUDP:
				return keyErr(a.Path, key+".expose", "%s is HTTP over TCP, and this service is %s", ExposeHTTPS, ProtocolUDP)
			case a.Domain == "":
				return keyErr(a.Path, key+".expose", "there is no %q to build a hostname from: set it, or use %q",
					"domain", DomainAuto)
			}
		default:

			if _, err := ParseExpose(string(s.Expose)); err != nil {
				return keyErr(a.Path, key+".expose", "%v", err)
			}
		}
	}
	return nil
}

func parseExposeNode(node *yaml.Node, path, key string) (Expose, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	e, err := ParseExpose(s)
	if err != nil {
		return "", keyErr(path, key, "%v", err)
	}
	return e, nil
}

func parseReplicas(node *yaml.Node, path, key string) (int, error) {
	n, err := scalarInt(node, path, key)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > MaxReplicas {
		return 0, keyErr(path, key, "%d replicas is out of range (1-%d)", n, MaxReplicas)
	}
	return n, nil
}

func parseDrain(node *yaml.Node, path, key string) (time.Duration, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return 0, err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, keyErr(path, key, "must be a duration such as %q or %q, got %q", "30s", "2m", s)
	}
	if d < 0 {
		return 0, keyErr(path, key, "must not be negative, got %q", s)
	}
	return d, nil
}
