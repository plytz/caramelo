package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const FileName = "caramelo.yaml"

type App struct {
	Name string `json:"name"`

	Domain string `json:"domain,omitempty"`

	Deps []Dep `json:"deps,omitempty"`

	Services []Service `json:"services,omitempty"`

	Test string `json:"test,omitempty"`

	Deploy *Deploy `json:"deploy,omitempty"`

	Envs map[string]EnvOverride `json:"envs,omitempty"`

	Placement Placement `json:"placement,omitempty"`

	Env map[string]string `json:"env,omitempty"`

	Path string `json:"path,omitempty"`
}

type Dep struct {
	Name string `json:"name"`

	Image string `json:"image"`

	Port int `json:"port"`

	Env map[string]string `json:"env,omitempty"`

	Ready []string `json:"ready,omitempty"`

	Data string `json:"data,omitempty"`
}

func (a *App) Dep(name string) (Dep, bool) {
	if a == nil {
		return Dep{}, false
	}
	for _, d := range a.Deps {
		if d.Name == name {
			return d, true
		}
	}
	return Dep{}, false
}

var ManagedVars = []string{"PORT", "CARAMELO_APP", "CARAMELO_ENV", "CARAMELO_SERVICE"}

func Managed(key string) bool {
	for _, k := range ManagedVars {
		if k == key {
			return true
		}
	}
	return false
}

var ReservedKeys = []string{"backups", "alerts"}

type ReservedKeyError struct {
	Key  string
	Path string
}

func (e *ReservedKeyError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %q is not supported yet", e.Path, e.Key)
	}
	return fmt.Sprintf("%q is not supported yet", e.Key)
}

func Reserved(key string) bool {
	for _, k := range ReservedKeys {
		if k == key {
			return true
		}
	}
	return false
}

var topLevelKeys = []string{"name", "domain", "deps", "services", "run", "install", "build", "test", "env",
	"deploy", "envs", "placement"}

var depKeys = []string{"image", "port", "env", "ready", "data"}

const (
	MaxSlugLen = 32

	SlugPattern = `^[a-z0-9][a-z0-9-]{0,31}$`
)

var slugRe = regexp.MustCompile(SlugPattern)

func validSlug(s string) bool { return slugRe.MatchString(s) }

var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Default(dir string) *App { return &App{Name: DefaultName(dir)} }

func DefaultName(dir string) string {
	base := filepath.Base(strings.TrimSpace(dir))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) > MaxSlugLen {
		name = strings.TrimRight(name[:MaxSlugLen], "-")
	}
	if !validSlug(name) {
		return "app"
	}
	return name
}

func Load(path string) (*App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", FileName, err)
	}
	return parse(data, path)
}

func LoadDir(dir string) (*App, error) {
	app, err := Load(filepath.Join(dir, FileName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Default(dir), nil
	case err != nil:
		return nil, err
	}
	if app.Name == "" {
		app.Name = DefaultName(dir)
	}
	return app, app.Validate()
}

func Parse(data []byte) (*App, error) { return parse(data, "") }

func parse(data []byte, path string) (*App, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, keyErr(path, "", "%v", err)
	}
	app := &App{Path: path}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return app, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == nullTag {
		return app, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, keyErr(path, "", "must be a mapping of %s", quoteList(topLevelKeys))
	}
	var short shorthand
	sawServices := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		var err error
		switch key := k.Value; {
		case key == "name":
			app.Name, err = scalarString(v, path, "name")
		case key == "domain":
			app.Domain, err = scalarString(v, path, "domain")
		case key == "deps":
			app.Deps, err = parseDeps(v, path)
		case key == "services":
			app.Services, err = parseServices(v, path)
			sawServices = true
		case key == "run":
			short.service.Run, err = requiredText(v, path, "run")
			err = short.set(key, err)
		case key == "install":
			short.service.Install, err = requiredText(v, path, "install")
			err = short.set(key, err)
		case key == "build":
			short.service.Build, err = parseBuild(v, path, "build")
			err = short.set(key, err)
		case key == "test":
			app.Test, err = requiredText(v, path, "test")
		case key == "env":
			app.Env, err = parseVars(v, path, "env")
		case key == "deploy":
			app.Deploy, err = parseDeploy(v, path, "deploy")
		case key == "envs":
			app.Envs, err = parseEnvs(v, path)
		case key == "placement":
			app.Placement, err = parsePlacementNode(v, path, "placement")
		case Reserved(key):
			err = &ReservedKeyError{Key: key, Path: path}
		default:
			err = keyErr(path, "", "unknown key %q (known keys: %s)", key, quoteList(topLevelKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if err := short.apply(app, sawServices, path); err != nil {
		return nil, err
	}
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return app, nil
}

func parseDeps(node *yaml.Node, path string) ([]Dep, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, "deps", "must be a mapping of name to image or settings")
	}
	deps := make([]Dep, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		d, err := parseDep(node.Content[i].Value, node.Content[i+1], path)
		if err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, nil
}

func parseDep(name string, node *yaml.Node, path string) (Dep, error) {
	key := "deps." + name
	d := Dep{Name: name}
	var setPort, setReady, setData bool
	switch {
	case node.Kind == yaml.ScalarNode && node.Tag != nullTag:
		d.Image = node.Value
	case node.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			fk, fv := node.Content[i], node.Content[i+1]
			field := key + "." + fk.Value
			var err error
			switch fk.Value {
			case "image":
				d.Image, err = scalarString(fv, path, field)
			case "port":
				d.Port, err = scalarInt(fv, path, field)
				setPort = true
			case "env":
				d.Env, err = parseVars(fv, path, field)
			case "ready":
				d.Ready, err = parseReady(fv, path, field)
				setReady = true
			case "data":
				d.Data, err = scalarString(fv, path, field)
				setData = true
			default:
				err = keyErr(path, key, "unknown key %q (known keys: %s)", fk.Value, quoteList(depKeys))
			}
			if err != nil {
				return Dep{}, err
			}
		}
	default:
		return Dep{}, keyErr(path, key, "must be an image reference or a mapping of %s", quoteList(depKeys))
	}
	if d.Image == "" {
		return Dep{}, keyErr(path, key, "image is required")
	}

	if known, ok := Lookup(d.Image); ok {
		if !setPort {
			d.Port = known.Port
		}
		if !setReady {
			d.Ready = known.Ready
		}
		if !setData {
			d.Data = known.Data
		}
		d.Env = mergeVars(known.Env, d.Env)
	}
	if d.Port == 0 {
		return Dep{}, keyErr(path, key, "port is required: %s is not a known image", d.Image)
	}
	return d, nil
}

func parseVars(node *yaml.Node, path, key string) (map[string]string, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, keyErr(path, key, "must be a mapping of name to value")
	}
	vars := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			return nil, keyErr(path, key+"."+k.Value, "must be a string")
		}
		if v.Tag == nullTag {
			vars[k.Value] = ""
			continue
		}
		vars[k.Value] = v.Value
	}
	if len(vars) == 0 {
		return nil, nil
	}
	return vars, nil
}

func parseReady(node *yaml.Node, path, key string) ([]string, error) {
	if node.Kind == yaml.ScalarNode && node.Tag == nullTag {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, keyErr(path, key, "must be a list of arguments, e.g. [pg_isready, -U, postgres]")
	}
	argv := make([]string, 0, len(node.Content))
	for _, n := range node.Content {
		if n.Kind != yaml.ScalarNode || n.Tag == nullTag {
			return nil, keyErr(path, key, "must be a list of arguments, e.g. [pg_isready, -U, postgres]")
		}
		argv = append(argv, n.Value)
	}
	if len(argv) == 0 {
		return nil, nil
	}
	return argv, nil
}

func (a *App) Validate() error {
	if a == nil {
		return errors.New("config: nil app")
	}
	if a.Name != "" && !validSlug(a.Name) {
		return keyErr(a.Path, "name", "invalid app name %q: use lowercase letters, digits and dashes, starting with a letter or a digit, at most %d characters", a.Name, MaxSlugLen)
	}
	seen := make(map[string]bool, len(a.Deps))
	for _, d := range a.Deps {
		key := "deps." + d.Name
		switch {
		case d.Name == "":
			return keyErr(a.Path, "deps", "a dependency needs a name")
		case !validSlug(d.Name):
			return keyErr(a.Path, "deps", "invalid dependency name %q: use lowercase letters, digits and dashes, starting with a letter or a digit, at most %d characters", d.Name, MaxSlugLen)
		case seen[d.Name]:
			return keyErr(a.Path, "deps", "duplicate dependency %q", d.Name)
		}
		seen[d.Name] = true
		if strings.TrimSpace(d.Image) == "" {
			return keyErr(a.Path, key, "image is required")
		}
		if d.Port < 1 || d.Port > 65535 {
			return keyErr(a.Path, key+".port", "port %d is out of range (1-65535)", d.Port)
		}
		for _, arg := range d.Ready {
			if arg == "" {
				return keyErr(a.Path, key+".ready", "arguments must not be empty")
			}
		}
		if d.Data != "" && !strings.HasPrefix(d.Data, "/") {
			return keyErr(a.Path, key+".data", "must be an absolute path inside the container, got %q", d.Data)
		}
		if err := a.validateVars(key+".env", d.Env); err != nil {
			return err
		}
	}
	if err := a.validateServices(); err != nil {
		return err
	}
	if err := a.validateEdge(); err != nil {
		return err
	}
	if err := a.validateDeploy(); err != nil {
		return err
	}
	if err := a.validateFleet(); err != nil {
		return err
	}
	return a.validateVars("env", a.Env)
}

func (a *App) validateVars(key string, vars map[string]string) error {
	ctx := ExpandContext{
		App:      a.Name,
		Env:      "env",
		Deps:     make(map[string]DepAddr, len(a.Deps)),
		Services: make(map[string]DepAddr, len(a.Services)),
	}
	for _, d := range a.Deps {
		ctx.Deps[d.Name] = DepAddr{}
	}
	for _, s := range a.Services {
		ctx.Services[s.Name] = DepAddr{}
	}
	for _, name := range sortedKeys(vars) {
		if !varNameRe.MatchString(name) {
			return keyErr(a.Path, key, "invalid variable name %q", name)
		}
		if _, err := ExpandString(vars[name], ctx); err != nil {
			return keyErr(a.Path, key+"."+name, "%v", err)
		}
	}
	return nil
}

const nullTag = "!!null"

func keyErr(path, key, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	switch {
	case path != "" && key != "":
		return fmt.Errorf("%s: %s: %s", path, key, msg)
	case path != "":
		return fmt.Errorf("%s: %s", path, msg)
	case key != "":
		return fmt.Errorf("%s: %s", key, msg)
	}
	return errors.New(msg)
}

func scalarString(n *yaml.Node, path, key string) (string, error) {
	if n.Kind != yaml.ScalarNode {
		return "", keyErr(path, key, "must be a string")
	}
	if n.Tag == nullTag {
		return "", nil
	}
	return n.Value, nil
}

func scalarInt(n *yaml.Node, path, key string) (int, error) {
	if n.Kind != yaml.ScalarNode || n.Tag == nullTag {
		return 0, keyErr(path, key, "must be a number")
	}
	i, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil {
		return 0, keyErr(path, key, "must be a number, got %q", n.Value)
	}
	return i, nil
}

func mergeVars(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func quoteList(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strconv.Quote(k)
	}
	return strings.Join(out, ", ")
}
