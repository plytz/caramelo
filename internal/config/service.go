package config

import (
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

func ParseProtocol(s string) (Protocol, error) {
	switch Protocol(s) {
	case "":
		return ProtocolTCP, nil
	case ProtocolTCP:
		return ProtocolTCP, nil
	case ProtocolUDP:
		return ProtocolUDP, nil
	}
	return "", keyErr("", "protocol", "unknown protocol %q: want %s or %s", s, ProtocolTCP, ProtocolUDP)
}

const PortNone = -1

type Service struct {
	Name string `json:"name"`

	Image string `json:"image,omitempty"`

	Build *Build `json:"build,omitempty"`

	Install string `json:"install,omitempty"`

	Run string `json:"run,omitempty"`

	Port int `json:"port,omitempty"`

	Protocol Protocol `json:"protocol,omitempty"`

	Health *Health `json:"health,omitempty"`

	Env map[string]string `json:"env,omitempty"`

	ReplicaCount int `json:"replicas,omitempty"`

	Expose Expose `json:"expose,omitempty"`

	Drain time.Duration `json:"drain,omitempty"`

	Resources *Resources `json:"resources,omitempty"`
}

const DefaultServiceName = "web"

type Build struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

type Health struct {
	Path string `json:"path,omitempty"`

	Command []string `json:"command,omitempty"`
}

func (a *App) Service(name string) (Service, bool) {
	if a == nil {
		return Service{}, false
	}
	for _, s := range a.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

func (a *App) FirstService() (Service, bool) {
	if a == nil || len(a.Services) == 0 {
		return Service{}, false
	}
	return a.Services[0], true
}

var serviceKeys = []string{"image", "build", "install", "run", "port", "protocol", "health", "env",
	"replicas", "expose", "drain", "resources"}

var buildKeys = []string{"context", "dockerfile"}

const PortNoneValue = "none"

func parseServices(node *yaml.Node, path string) ([]Service, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, "services", "must be a mapping of name to settings")
	}
	services := make([]Service, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		s, err := parseService(node.Content[i].Value, node.Content[i+1], path)
		if err != nil {
			return nil, err
		}
		services = append(services, s)
	}
	if len(services) == 0 {
		return nil, nil
	}
	return services, nil
}

func parseService(name string, node *yaml.Node, path string) (Service, error) {
	key := "services." + name
	s := Service{Name: name}
	if node.Kind != yaml.MappingNode {
		return Service{}, keyErr(path, key, "must be a mapping of %s (for a single service, write the top-level %q instead)", quoteList(serviceKeys), "run")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		fk, fv := node.Content[i], node.Content[i+1]
		field := key + "." + fk.Value
		var err error
		switch fk.Value {
		case "image":
			s.Image, err = requiredText(fv, path, field)
		case "build":
			s.Build, err = parseBuild(fv, path, field)
		case "install":
			s.Install, err = requiredText(fv, path, field)
		case "run":
			s.Run, err = requiredText(fv, path, field)
		case "port":
			s.Port, err = parsePort(fv, path, field)
		case "protocol":
			s.Protocol, err = parseProtocolNode(fv, path, field)
		case "health":
			s.Health, err = parseHealth(fv, path, field)
		case "env":
			s.Env, err = parseVars(fv, path, field)
		case "replicas":
			s.ReplicaCount, err = parseReplicas(fv, path, field)
		case "expose":
			s.Expose, err = parseExposeNode(fv, path, field)
		case "drain":
			s.Drain, err = parseDrain(fv, path, field)
		case "resources":
			s.Resources, err = parseResources(fv, path, field)
		default:
			err = keyErr(path, key, "unknown key %q (known keys: %s)", fk.Value, quoteList(serviceKeys))
		}
		if err != nil {
			return Service{}, err
		}
	}
	return s, nil
}

func parseBuild(node *yaml.Node, path, key string) (*Build, error) {
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag != nullTag:
		if strings.TrimSpace(node.Value) == "" {
			return nil, keyErr(path, key, "must not be empty")
		}
		return &Build{Context: node.Value}, nil
	case node.Kind == yaml.MappingNode:
		b := &Build{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			fk, fv := node.Content[i], node.Content[i+1]
			field := key + "." + fk.Value
			var err error
			switch fk.Value {
			case "context":
				b.Context, err = requiredText(fv, path, field)
			case "dockerfile":
				b.Dockerfile, err = requiredText(fv, path, field)
			default:
				err = keyErr(path, key, "unknown key %q (known keys: %s)", fk.Value, quoteList(buildKeys))
			}
			if err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	return nil, keyErr(path, key, "must be a build context path or a mapping of %s", quoteList(buildKeys))
}

func parsePort(node *yaml.Node, path, key string) (int, error) {
	if node.Kind == yaml.ScalarNode && strings.TrimSpace(node.Value) == PortNoneValue {
		return PortNone, nil
	}
	n, err := scalarInt(node, path, key)
	if err != nil {
		return 0, keyErr(path, key, "must be a number or %q", PortNoneValue)
	}

	if n < 1 || n > 65535 {
		return 0, keyErr(path, key, "port %d is out of range (1-65535), or %q", n, PortNoneValue)
	}
	return n, nil
}

func parseProtocolNode(node *yaml.Node, path, key string) (Protocol, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	p, err := ParseProtocol(s)
	if err != nil {
		return "", keyErr(path, key, "unknown protocol %q: want %s or %s", s, ProtocolTCP, ProtocolUDP)
	}
	return p, nil
}

func parseHealth(node *yaml.Node, path, key string) (*Health, error) {
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag == nullTag:
		return nil, nil
	case node.Kind == yaml.ScalarNode:
		if !strings.HasPrefix(node.Value, "/") {
			return nil, keyErr(path, key, "must be an HTTP path starting with %q (e.g. /healthz) or a list of arguments", "/")
		}
		return &Health{Path: node.Value}, nil
	case node.Kind == yaml.SequenceNode:
		argv, err := parseReady(node, path, key)
		if err != nil {
			return nil, keyErr(path, key, "must be an HTTP path (e.g. /healthz) or a list of arguments, e.g. [pg_isready]")
		}
		if len(argv) == 0 {
			return nil, keyErr(path, key, "must not be empty: leave it out for the default check")
		}
		return &Health{Command: argv}, nil
	}
	return nil, keyErr(path, key, "must be an HTTP path (e.g. /healthz) or a list of arguments")
}

func requiredText(n *yaml.Node, path, key string) (string, error) {
	s, err := scalarString(n, path, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", keyErr(path, key, "must not be empty")
	}
	return s, nil
}

type shorthand struct {
	keys    []string
	service Service
}

func (s *shorthand) set(key string, err error) error {
	if err != nil {
		return err
	}
	s.keys = append(s.keys, key)
	return nil
}

func (s *shorthand) apply(app *App, sawServices bool, path string) error {
	if len(s.keys) == 0 {
		return nil
	}
	if sawServices {
		return keyErr(path, "", "%s and %q are mutually exclusive: %s describes a single service called %q, so write it under services.%s instead",
			quoteList(s.keys), "services", quoteList(s.keys), DefaultServiceName, DefaultServiceName)
	}
	s.service.Name = DefaultServiceName
	app.Services = []Service{s.service}
	return nil
}

func (s Service) ContainerPort(blockPort int) int {
	switch s.Port {
	case PortNone:
		return 0
	case 0:
		return blockPort
	}
	return s.Port
}

func (s Service) HasPort() bool { return s.Port != PortNone }

func (a *App) ServiceEnv(s Service) map[string]string {
	if a == nil {
		return mergeVars(nil, s.Env)
	}
	return mergeVars(a.Env, s.Env)
}

func (a *App) validateServices() error {
	seen := make(map[string]bool, len(a.Services))
	for _, s := range a.Services {
		key := "services." + s.Name
		switch {
		case s.Name == "":
			return keyErr(a.Path, "services", "a service needs a name")
		case !validSlug(s.Name):
			return keyErr(a.Path, "services", "invalid service name %q: use lowercase letters, digits and dashes, starting with a letter or a digit, at most %d characters", s.Name, MaxSlugLen)
		case seen[s.Name]:
			return keyErr(a.Path, "services", "duplicate service %q", s.Name)
		}
		seen[s.Name] = true

		if _, ok := a.Dep(s.Name); ok {
			return keyErr(a.Path, key, "%q is also a dependency: services and dependencies share one set of names on the env's network", s.Name)
		}
		if s.Image != "" && s.Build != nil {
			return keyErr(a.Path, key, "%q and %q are mutually exclusive: an image is run as it is, a build context is built into one", "image", "build")
		}
		if err := validateImage(a.Path, key, s.Image); err != nil {
			return err
		}

		if s.Build != nil && strings.TrimSpace(s.Install) != "" {
			return keyErr(a.Path, key, "%q and %q are mutually exclusive: a service built from a Dockerfile installs what it needs in the build (put the command in the Dockerfile)", "install", "build")
		}
		if err := validateBuild(a.Path, key, s.Build); err != nil {
			return err
		}
		if s.Port != PortNone && (s.Port < 0 || s.Port > 65535) {
			return keyErr(a.Path, key+".port", "port %d is out of range (1-65535), or %q", s.Port, PortNoneValue)
		}
		if _, err := ParseProtocol(string(s.Protocol)); err != nil {
			return keyErr(a.Path, key+".protocol", "unknown protocol %q: want %s or %s", s.Protocol, ProtocolTCP, ProtocolUDP)
		}
		if err := validateHealth(a.Path, key, s); err != nil {
			return err
		}
		if err := a.validateVars(key+".env", s.Env); err != nil {
			return err
		}
	}
	return nil
}

func validateImage(path, key, image string) error {
	if image == "" {
		return nil
	}
	if strings.HasPrefix(image, "-") {
		return keyErr(path, key+".image", "an image reference does not start with %q, got %q", "-", image)
	}
	if strings.ContainsFunc(image, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune(`"'$&|;<>(){}[]*?!\`+"`", r)
	}) {
		return keyErr(path, key+".image", "%q is not an image reference: use name[:tag] or name@digest", image)
	}
	return nil
}

func validateBuild(path, key string, b *Build) error {
	if b == nil {
		return nil
	}
	if strings.TrimSpace(b.Context) == "" {
		return keyErr(path, key+".build.context", "a build needs a context path, e.g. %q", ".")
	}
	for _, f := range []struct{ name, value string }{{"context", b.Context}, {"dockerfile", b.Dockerfile}} {
		if f.value == "" {
			continue
		}
		if strings.HasPrefix(f.value, "/") {
			return keyErr(path, key+".build."+f.name, "must be relative to the checkout, got %q", f.value)
		}
		if escapes(f.value) {
			return keyErr(path, key+".build."+f.name, "must stay inside the checkout, got %q", f.value)
		}
	}
	return nil
}

func escapes(p string) bool {
	clean := filepath.ToSlash(filepath.Clean(p))
	return clean == ".." || strings.HasPrefix(clean, "../")
}

func validateHealth(path, key string, s Service) error {
	h := s.Health
	if h == nil {
		return nil
	}
	switch {
	case h.Path != "" && len(h.Command) > 0:
		return keyErr(path, key+".health", "is either an HTTP path or a command, not both")
	case h.Path == "" && len(h.Command) == 0:
		return keyErr(path, key+".health", "must be an HTTP path (e.g. /healthz) or a list of arguments")
	case h.Path != "":
		switch {
		case !strings.HasPrefix(h.Path, "/"):
			return keyErr(path, key+".health", "an HTTP path starts with %q, got %q", "/", h.Path)
		case !s.HasPort():
			return keyErr(path, key+".health", "an HTTP check needs a port, and this service wrote %q: use a command instead", "port: "+PortNoneValue)
		case s.Protocol == ProtocolUDP:
			return keyErr(path, key+".health", "an HTTP check needs a TCP port, and this service is %s: use a command instead", ProtocolUDP)
		}
	default:
		for _, arg := range h.Command {
			if arg == "" {
				return keyErr(path, key+".health", "arguments must not be empty")
			}
		}
	}
	return nil
}
