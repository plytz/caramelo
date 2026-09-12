package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultWatch = 60 * time.Second

	DefaultMaxErrors = "5%"

	MaxErrorsNone = "none"

	MinErrorRequests = 20

	DefaultKeep = 5

	MaxKeep = 100
)

type Promote string

const (
	PromoteAuto Promote = "auto"

	PromoteManual Promote = "manual"
)

var Promotes = []Promote{PromoteAuto, PromoteManual}

const DefaultPromote = PromoteAuto

func ParsePromote(s string) (Promote, error) {
	switch p := Promote(strings.TrimSpace(s)); p {
	case "":
		return "", nil
	case PromoteAuto, PromoteManual:
		return p, nil
	}
	return "", fmt.Errorf("unknown promote %q: want %s or %s", s, PromoteAuto, PromoteManual)
}

func (p Promote) String() string { return string(p) }

type Deploy struct {
	Before string `json:"before,omitempty"`

	Check string `json:"check,omitempty"`

	Watch time.Duration `json:"watch,omitempty"`

	MaxErrors string `json:"max_errors,omitempty"`

	Promote Promote `json:"promote,omitempty"`

	Keep int `json:"keep,omitempty"`
}

var deployKeys = []string{"before", "check", "watch", "max_errors", "promote", "keep"}

func (d *Deploy) WatchWindow() time.Duration {
	if d == nil || d.Watch <= 0 {
		return DefaultWatch
	}
	return d.Watch
}

func (d *Deploy) MaxErrorRate() float64 {
	s := DefaultMaxErrors
	if d != nil && strings.TrimSpace(d.MaxErrors) != "" {
		s = d.MaxErrors
	}
	rate, err := ParseMaxErrors(s)
	if err != nil {
		rate, _ = ParseMaxErrors(DefaultMaxErrors)
	}
	return rate
}

func (d *Deploy) PromotePolicy() Promote {
	if d == nil || d.Promote == "" {
		return DefaultPromote
	}
	return d.Promote
}

func (d *Deploy) KeepReleases() int {
	if d == nil || d.Keep <= 0 {
		return DefaultKeep
	}
	return d.Keep
}

func (d *Deploy) Merge(over *Deploy) *Deploy {
	if d == nil && over == nil {
		return nil
	}
	out := Deploy{}
	if d != nil {
		out = *d
	}
	if over == nil {
		return &out
	}
	if over.Before != "" {
		out.Before = over.Before
	}
	if over.Check != "" {
		out.Check = over.Check
	}
	if over.Watch > 0 {
		out.Watch = over.Watch
	}
	if strings.TrimSpace(over.MaxErrors) != "" {
		out.MaxErrors = over.MaxErrors
	}
	if over.Promote != "" {
		out.Promote = over.Promote
	}
	if over.Keep > 0 {
		out.Keep = over.Keep
	}
	return &out
}

const NoMaxErrors = -1.0

func ParseMaxErrors(s string) (float64, error) {
	if strings.EqualFold(strings.TrimSpace(s), MaxErrorsNone) {
		return NoMaxErrors, nil
	}
	rate, err := ParsePercent(s)
	if err != nil {
		return 0, fmt.Errorf("%w, or %q", err, MaxErrorsNone)
	}
	return rate, nil
}

func ParsePercent(s string) (float64, error) {
	t := strings.TrimSpace(s)
	num, ok := strings.CutSuffix(t, "%")
	if !ok {
		return 0, fmt.Errorf("must be a percentage such as %q, got %q", DefaultMaxErrors, s)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("must be a percentage such as %q, got %q", DefaultMaxErrors, s)
	}
	if v < 0 || v > 100 {
		return 0, fmt.Errorf("%q is out of range (0%% to 100%%)", s)
	}
	return v / 100, nil
}

type Resources struct {
	Memory int64 `json:"memory,omitempty"`

	CPU float64 `json:"cpu,omitempty"`
}

var resourceKeys = []string{"memory", "cpu"}

const MinMemoryBytes int64 = 6 << 20

func (r Resources) Empty() bool { return r.Memory == 0 && r.CPU == 0 }

func (r Resources) Merge(over Resources) Resources {
	if over.Memory != 0 {
		r.Memory = over.Memory
	}
	if over.CPU != 0 {
		r.CPU = over.CPU
	}
	return r
}

func (r Resources) MemoryArg() string {
	if r.Memory <= 0 {
		return ""
	}
	return strconv.FormatInt(r.Memory, 10)
}

func (r Resources) CPUArg() string {
	if r.CPU <= 0 {
		return ""
	}
	return strconv.FormatFloat(r.CPU, 'f', -1, 64)
}

func (r Resources) String() string {
	switch {
	case r.Empty():
		return ""
	case r.Memory == 0:
		return "cpu " + r.CPUArg()
	case r.CPU == 0:
		return FormatMemory(r.Memory)
	}
	return FormatMemory(r.Memory) + ", cpu " + r.CPUArg()
}

var memoryUnits = []struct {
	suffix string
	factor int64
}{
	{"gb", 1 << 30}, {"g", 1 << 30},
	{"mb", 1 << 20}, {"m", 1 << 20},
	{"kb", 1 << 10}, {"k", 1 << 10},
	{"b", 1}, {"", 1},
}

func ParseMemory(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("must be a size such as %q or %q", "512m", "1g")
	}
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, unit := t[:i], strings.TrimSpace(t[i:])
	if num == "" {
		return 0, fmt.Errorf("must be a size such as %q or %q, got %q", "512m", "1g", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a size such as %q or %q, got %q", "512m", "1g", s)
	}
	for _, u := range memoryUnits {
		if unit != u.suffix {
			continue
		}
		bytes := int64(v * float64(u.factor))
		if bytes <= 0 {
			return 0, fmt.Errorf("must be a positive size, got %q", s)
		}
		return bytes, nil
	}
	return 0, fmt.Errorf("unknown size unit %q: want one of b, k, m, g (as in %q)", unit, "512m")
}

func FormatMemory(b int64) string {
	if b <= 0 {
		return ""
	}
	for _, u := range []struct {
		suffix string
		factor int64
	}{{"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}} {
		if b%u.factor == 0 {
			return strconv.FormatInt(b/u.factor, 10) + u.suffix
		}
	}
	return strconv.FormatInt(b, 10)
}

type EnvOverride struct {
	Hosts []string `json:"hosts,omitempty"`

	Services map[string]ServiceOverride `json:"services,omitempty"`

	Deploy *Deploy `json:"deploy,omitempty"`

	Machine string `json:"machine,omitempty"`

	Via Via `json:"via,omitempty"`
}

type ServiceOverride struct {
	Replicas int `json:"replicas,omitempty"`

	Resources *Resources `json:"resources,omitempty"`
}

var (
	envOverrideKeys     = []string{"hosts", "services", "deploy", "machine", "via"}
	serviceOverrideKeys = []string{"replicas", "resources"}
)

func (o EnvOverride) Empty() bool {
	return len(o.Hosts) == 0 && len(o.Services) == 0 && o.Deploy == nil &&
		o.Machine == "" && o.Via == ""
}

func (a *App) Override(env string) (EnvOverride, bool) {
	if a == nil || len(a.Envs) == 0 {
		return EnvOverride{}, false
	}
	o, ok := a.Envs[env]
	return o, ok
}

func (a *App) ForEnv(env string) *App {
	if a == nil {
		return nil
	}
	out := *a
	out.Services = append([]Service(nil), a.Services...)
	out.Deps = append([]Dep(nil), a.Deps...)

	for i := range out.Services {
		if r := out.Services[i].Resources; r != nil {
			c := *r
			out.Services[i].Resources = &c
		}
	}
	o, ok := a.Override(env)
	if !ok || o.Empty() {
		return &out
	}
	out.Deploy = a.Deploy.Merge(o.Deploy)
	for i := range out.Services {
		so, ok := o.Services[out.Services[i].Name]
		if !ok {
			continue
		}
		if so.Replicas > 0 {
			out.Services[i].ReplicaCount = so.Replicas
		}
		if so.Resources != nil {
			merged := Resources{}
			if out.Services[i].Resources != nil {
				merged = *out.Services[i].Resources
			}
			merged = merged.Merge(*so.Resources)
			out.Services[i].Resources = &merged
		}
	}
	return &out
}

func (a *App) EnvHosts(env string) []string {
	o, ok := a.Override(env)
	if !ok || len(o.Hosts) == 0 {
		return nil
	}
	return append([]string(nil), o.Hosts...)
}

func (a *App) HostsFor(env, service string) []string {
	if a == nil || env == "" {
		return nil
	}
	if a.ExposeOf(service) != ExposeHTTPS {
		return nil
	}
	if hosts := a.EnvHosts(env); len(hosts) > 0 && a.isFirstExposed(service) {
		return hosts
	}
	if h := a.HostFor(env, service); h != "" {
		return []string{h}
	}
	return nil
}

func parseDeploy(node *yaml.Node, path, key string) (*Deploy, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, key, "must be a mapping of %s", quoteList(deployKeys))
	}
	d := &Deploy{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		field := key + "." + k.Value
		var err error
		switch k.Value {
		case "before":
			d.Before, err = requiredText(v, path, field)
		case "check":
			d.Check, err = requiredText(v, path, field)
		case "watch":
			d.Watch, err = parseDuration(v, path, field)
		case "max_errors":
			d.MaxErrors, err = parsePercentNode(v, path, field)
		case "promote":
			d.Promote, err = parsePromoteNode(v, path, field)
		case "keep":
			d.Keep, err = parseKeep(v, path, field)
		default:
			err = keyErr(path, key, "unknown key %q (known keys: %s)", k.Value, quoteList(deployKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}

func parseDuration(node *yaml.Node, path, key string) (time.Duration, error) {
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
		return 0, keyErr(path, key, "must be a duration such as %q or %q, got %q", "60s", "5m", s)
	}
	if d < 0 {
		return 0, keyErr(path, key, "must not be negative, got %q", s)
	}
	return d, nil
}

func parsePercentNode(node *yaml.Node, path, key string) (string, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := ParseMaxErrors(s); err != nil {
		return "", keyErr(path, key, "%v", err)
	}
	return s, nil
}

func parsePromoteNode(node *yaml.Node, path, key string) (Promote, error) {
	s, err := scalarString(node, path, key)
	if err != nil {
		return "", err
	}
	p, err := ParsePromote(s)
	if err != nil {
		return "", keyErr(path, key, "%v", err)
	}
	return p, nil
}

func parseKeep(node *yaml.Node, path, key string) (int, error) {
	n, err := scalarInt(node, path, key)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > MaxKeep {
		return 0, keyErr(path, key, "%d is out of range (1-%d)", n, MaxKeep)
	}
	return n, nil
}

func parseResources(node *yaml.Node, path, key string) (*Resources, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, key, "must be a mapping of %s", quoteList(resourceKeys))
	}
	r := &Resources{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		field := key + "." + k.Value
		switch k.Value {
		case "memory":
			s, err := scalarString(v, path, field)
			if err != nil {
				return nil, err
			}
			bytes, err := ParseMemory(s)
			if err != nil {
				return nil, keyErr(path, field, "%v", err)
			}
			r.Memory = bytes
		case "cpu":
			s, err := scalarString(v, path, field)
			if err != nil {
				return nil, err
			}
			cpu, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return nil, keyErr(path, field, "must be a number of CPUs such as %q or %q, got %q", "1", "0.5", s)
			}
			r.CPU = cpu
		default:
			return nil, keyErr(path, key, "unknown key %q (known keys: %s)", k.Value, quoteList(resourceKeys))
		}
	}
	if r.Empty() {
		return nil, nil
	}
	return r, nil
}

func parseEnvs(node *yaml.Node, path string) (map[string]EnvOverride, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, "envs", "must be a mapping of environment name to overrides")
	}
	out := make(map[string]EnvOverride, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if _, dup := out[name]; dup {
			return nil, keyErr(path, "envs", "duplicate environment %q", name)
		}
		o, err := parseEnvOverride(name, node.Content[i+1], path)
		if err != nil {
			return nil, err
		}
		out[name] = o
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func parseEnvOverride(name string, node *yaml.Node, path string) (EnvOverride, error) {
	key := "envs." + name
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return EnvOverride{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return EnvOverride{}, keyErr(path, key, "must be a mapping of %s", quoteList(envOverrideKeys))
	}
	o := EnvOverride{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		field := key + "." + k.Value
		var err error
		switch k.Value {
		case "hosts":
			o.Hosts, err = parseHosts(v, path, field)
		case "services":
			o.Services, err = parseServiceOverrides(v, path, field)
		case "deploy":
			o.Deploy, err = parseDeploy(v, path, field)
		case "machine":
			o.Machine, err = parseMachineNode(v, path, field)
		case "via":
			o.Via, err = parseViaNode(v, path, field)
		default:
			err = keyErr(path, key, "unknown key %q: an environment may only override %s",
				k.Value, quoteList(envOverrideKeys))
		}
		if err != nil {
			return EnvOverride{}, err
		}
	}
	return o, nil
}

func parseHosts(node *yaml.Node, path, key string) ([]string, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, keyErr(path, key, "must be a list of hostnames, e.g. [shop.example.com, www.shop.example.com]")
	}
	out := make([]string, 0, len(node.Content))
	for _, n := range node.Content {
		if n.Kind != yaml.ScalarNode || n.Tag == nullTag {
			return nil, keyErr(path, key, "must be a list of hostnames, e.g. [shop.example.com]")
		}
		host := strings.TrimSpace(n.Value)
		if err := ValidateHostname(host); err != nil {
			return nil, keyErr(path, key, "%v", err)
		}
		out = append(out, host)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func parseServiceOverrides(node *yaml.Node, path, key string) (map[string]ServiceOverride, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, key, "must be a mapping of service name to %s", quoteList(serviceOverrideKeys))
	}
	out := make(map[string]ServiceOverride, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		field := key + "." + name
		if _, dup := out[name]; dup {
			return nil, keyErr(path, key, "duplicate service %q", name)
		}
		o, err := parseServiceOverride(node.Content[i+1], path, field)
		if err != nil {
			return nil, err
		}
		out[name] = o
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func parseServiceOverride(node *yaml.Node, path, key string) (ServiceOverride, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return ServiceOverride{}, nil
	}
	if node.Kind != yaml.MappingNode {
		return ServiceOverride{}, keyErr(path, key, "must be a mapping of %s", quoteList(serviceOverrideKeys))
	}
	o := ServiceOverride{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		field := key + "." + k.Value
		var err error
		switch k.Value {
		case "replicas":
			o.Replicas, err = parseReplicas(v, path, field)
		case "resources":
			o.Resources, err = parseResources(v, path, field)
		default:
			err = keyErr(path, key, "unknown key %q: an environment may only override %s of a service",
				k.Value, quoteList(serviceOverrideKeys))
		}
		if err != nil {
			return ServiceOverride{}, err
		}
	}
	return o, nil
}

func (a *App) validateDeploy() error {
	if err := validateDeployBlock(a.Path, "deploy", a.Deploy); err != nil {
		return err
	}
	for _, s := range a.Services {
		if err := validateResources(a.Path, "services."+s.Name+".resources", s.Resources); err != nil {
			return err
		}
	}
	for _, name := range sortedEnvNames(a.Envs) {
		o := a.Envs[name]
		key := "envs." + name
		if !validSlug(name) {
			return keyErr(a.Path, "envs", "invalid environment name %q: use lowercase letters, digits and dashes, starting with a letter or a digit, at most %d characters", name, MaxSlugLen)
		}
		if err := validateDeployBlock(a.Path, key+".deploy", o.Deploy); err != nil {
			return err
		}
		seen := make(map[string]bool, len(o.Hosts))
		for _, h := range o.Hosts {
			if err := ValidateHostname(h); err != nil {
				return keyErr(a.Path, key+".hosts", "%v", err)
			}
			if seen[h] {
				return keyErr(a.Path, key+".hosts", "duplicate hostname %q", h)
			}
			seen[h] = true
		}
		for _, svc := range sortedServiceOverrides(o.Services) {
			so := o.Services[svc]
			skey := key + ".services." + svc
			if _, ok := a.Service(svc); !ok {
				return keyErr(a.Path, skey, "there is no service %q in this app (services: %s)",
					svc, quoteList(serviceNames(a)))
			}
			if so.Replicas != 0 && (so.Replicas < 1 || so.Replicas > MaxReplicas) {
				return keyErr(a.Path, skey+".replicas", "%d replicas is out of range (1-%d)", so.Replicas, MaxReplicas)
			}
			if err := validateResources(a.Path, skey+".resources", so.Resources); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDeployBlock(path, key string, d *Deploy) error {
	if d == nil {
		return nil
	}
	if d.Watch < 0 {
		return keyErr(path, key+".watch", "must not be negative, got %s", d.Watch)
	}
	if s := strings.TrimSpace(d.MaxErrors); s != "" {
		if _, err := ParseMaxErrors(s); err != nil {
			return keyErr(path, key+".max_errors", "%v", err)
		}
	}
	if d.Promote != "" {
		if _, err := ParsePromote(string(d.Promote)); err != nil {
			return keyErr(path, key+".promote", "%v", err)
		}
	}
	if d.Keep != 0 && (d.Keep < 1 || d.Keep > MaxKeep) {
		return keyErr(path, key+".keep", "%d is out of range (1-%d)", d.Keep, MaxKeep)
	}
	return nil
}

func validateResources(path, key string, r *Resources) error {
	if r == nil {
		return nil
	}
	if r.Memory < 0 {
		return keyErr(path, key+".memory", "must not be negative")
	}
	if r.Memory > 0 && r.Memory < MinMemoryBytes {
		return keyErr(path, key+".memory", "%s is below the %s Docker accepts as a memory limit",
			FormatMemory(r.Memory), FormatMemory(MinMemoryBytes))
	}
	if r.CPU < 0 {
		return keyErr(path, key+".cpu", "must not be negative")
	}
	return nil
}

func sortedEnvNames(m map[string]EnvOverride) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedServiceOverrides(m map[string]ServiceOverride) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func serviceNames(a *App) []string {
	out := make([]string, 0, len(a.Services))
	for _, s := range a.Services {
		out = append(out, s.Name)
	}
	return out
}
