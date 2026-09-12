package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var detectors = []func(*checkout) *Guess{
	detectDocker,
	detectGo,
	detectRails,
	detectNode,
	detectPython,
}

const (
	defaultGoImage     = "golang:1.23"
	defaultNodeVersion = "22"
	defaultPythonImage = "python:3.12"
	defaultRubyVersion = "3.3"
)

const (
	cacheGo     = "/go/pkg/mod"
	cacheNPM    = "/root/.npm"
	cachePNPM   = "/root/.local/share/pnpm/store"
	cacheYarn   = "/usr/local/share/.cache/yarn"
	cachePip    = "/root/.cache/pip"
	cacheBundle = "/usr/local/bundle"
)

func Detect(dir string) (*Guess, error) {
	c, err := newCheckout(dir)
	if err != nil {
		return nil, err
	}
	for _, detect := range detectors {
		if g := detect(c); g != nil {
			return g, nil
		}
	}
	return nil, nil
}

type checkout struct {
	dir   string
	files map[string]bool
}

func newCheckout(dir string) (*checkout, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("detect stack in %s: %w", dir, err)
	}
	c := &checkout{dir: dir, files: make(map[string]bool, len(entries))}
	for _, e := range entries {

		c.files[e.Name()] = true
	}
	return c, nil
}

func (c *checkout) has(name string) bool { return c.files[name] }

func (c *checkout) read(name string) (string, bool) {
	if !c.has(name) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func detectDocker(c *checkout) *Guess {
	if !c.has("Dockerfile") {
		return nil
	}
	return &Guess{
		Stack: Docker,
		Build: Field{Value: ".", Evidence: "Dockerfile at the top level"},

		Run: Field{Evidence: "the image's own CMD"},
	}
}

var goDirectiveRe = regexp.MustCompile(`(?m)^\s*go\s+([0-9]+(?:\.[0-9]+){0,2})\s*$`)

func detectGo(c *checkout) *Guess {
	if !c.has("go.mod") {
		return nil
	}
	image, evidence := defaultGoImage, "go.mod has no go directive"
	if body, ok := c.read("go.mod"); ok {
		if m := goDirectiveRe.FindStringSubmatch(body); m != nil {
			image = "golang:" + m[1]
			evidence = "go.mod says go " + m[1]
		}
	}
	return &Guess{
		Stack:   Go,
		Image:   Field{Value: image, Evidence: evidence},
		Install: Field{Value: "go mod download", Evidence: "go.mod"},
		Run:     Field{Value: "go run .", Evidence: "go.mod at the top level"},
		Test:    Field{Value: "go test ./...", Evidence: "go.mod"},
		Cache:   cacheGo,
	}
}

var nodeVersionRe = regexp.MustCompile(`[0-9]+`)

type packageJSON struct {
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
	Scripts map[string]string `json:"scripts"`
}

func detectNode(c *checkout) *Guess {
	if !c.has("package.json") {
		return nil
	}
	var pkg packageJSON
	body, ok := c.read("package.json")
	readable := ok && json.Unmarshal([]byte(body), &pkg) == nil

	image := Field{Value: "node:" + defaultNodeVersion}
	switch {
	case !readable:
		image.Evidence = "package.json could not be read"
	case pkg.Engines.Node == "":
		image.Evidence = "package.json has no engines.node"
	default:
		if m := nodeVersionRe.FindString(pkg.Engines.Node); m != "" {
			image.Value = "node:" + m
			image.Evidence = fmt.Sprintf("package.json says engines.node %s", pkg.Engines.Node)
		} else {
			image.Evidence = fmt.Sprintf("package.json says engines.node %s, which names no version", pkg.Engines.Node)
		}
	}

	pm, install, cache := nodePackageManager(c)
	g := &Guess{
		Stack:   Node,
		Image:   image,
		Install: install,
		Cache:   cache,
	}
	switch {
	case !readable:
		g.Run = Field{Evidence: "package.json could not be read"}
	case pkg.Scripts["start"] != "":
		g.Run = Field{Value: pm + " start", Evidence: "package.json has a start script"}
	case pkg.Scripts["dev"] != "":
		g.Run = Field{Value: pm + " run dev", Evidence: "package.json has a dev script and no start script"}
	default:
		g.Run = Field{Evidence: "package.json has neither a start nor a dev script"}
	}
	if readable && pkg.Scripts["test"] != "" {
		g.Test = Field{Value: pm + " test", Evidence: "package.json has a test script"}
	}
	return g
}

func nodePackageManager(c *checkout) (pm string, install Field, cache string) {
	switch {
	case c.has("pnpm-lock.yaml"):
		return "pnpm", Field{Value: "pnpm install --frozen-lockfile", Evidence: "pnpm-lock.yaml"}, cachePNPM
	case c.has("yarn.lock"):
		return "yarn", Field{Value: "yarn install --frozen-lockfile", Evidence: "yarn.lock"}, cacheYarn
	case c.has("package-lock.json"):
		return "npm", Field{Value: "npm ci", Evidence: "package-lock.json"}, cacheNPM
	}

	return "npm", Field{Value: "npm install", Evidence: "no lockfile"}, cacheNPM
}

var railsGemRe = regexp.MustCompile(`(?m)^\s*gem\s+['"]rails['"]`)

func detectRails(c *checkout) *Guess {
	body, ok := c.read("Gemfile")
	if !ok || !railsGemRe.MatchString(body) {
		return nil
	}
	image := Field{Value: "ruby:" + defaultRubyVersion, Evidence: "Gemfile says rails, no .ruby-version"}
	if v, ok := c.read(".ruby-version"); ok {
		if version := rubyVersion(v); version != "" {
			image = Field{Value: "ruby:" + version, Evidence: ".ruby-version says " + version}
		}
	}
	return &Guess{
		Stack:   Rails,
		Image:   image,
		Install: Field{Value: "bundle install", Evidence: "Gemfile"},
		Run:     Field{Value: "bin/rails server -b 0.0.0.0 -p $PORT", Evidence: "Gemfile says rails"},
		Test:    Field{Value: "bin/rails test", Evidence: "Gemfile says rails"},
		Cache:   cacheBundle,
	}
}

var rubyVersionRe = regexp.MustCompile(`^(?:ruby-)?([0-9]+(?:\.[0-9]+){0,2})$`)

func rubyVersion(s string) string {
	m := rubyVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	return m[1]
}

func detectPython(c *checkout) *Guess {
	hasReq, hasProject := c.has("requirements.txt"), c.has("pyproject.toml")
	if !hasReq && !hasProject {
		return nil
	}
	evidence := "pyproject.toml"
	install := Field{Value: "pip install -e .", Evidence: "pyproject.toml"}
	if hasReq {
		evidence = "requirements.txt"
		install = Field{Value: "pip install -r requirements.txt", Evidence: "requirements.txt"}
	}
	g := &Guess{
		Stack:   Python,
		Image:   Field{Value: defaultPythonImage, Evidence: evidence},
		Install: install,
		Test:    Field{Value: "pytest", Evidence: evidence},
		Cache:   cachePip,
	}
	if c.has("manage.py") {
		g.Run = Field{Value: "python manage.py runserver 0.0.0.0:$PORT", Evidence: "manage.py: a Django project"}
	} else {

		g.Run = Field{Evidence: "no manage.py: only caramelo.yaml can say how to run this"}
	}
	return g
}
