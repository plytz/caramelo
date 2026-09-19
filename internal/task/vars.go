package task

import (
	"fmt"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/userdir"
)

const (
	VarHostname     = "Hostname"
	VarOS           = "OS"
	VarArch         = "Arch"
	VarUser         = "User"
	VarConfigDir    = "ConfigDir"
	VarCacheDir     = "CacheDir"
	VarCommanderDir = "CommanderDir"
)

var EngineVars = []string{VarHostname, VarOS, VarArch, VarUser, VarConfigDir, VarCacheDir, VarCommanderDir}

func MachineVars() (map[string]string, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read this machine's hostname: %w", err)
	}
	config, err := userdir.Config()
	if err != nil {
		return nil, err
	}
	cache, err := userdir.Cache()
	if err != nil {
		return nil, err
	}
	commander, err := remote.CommanderDir()
	if err != nil {
		return nil, err
	}
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return map[string]string{
		VarHostname:     host,
		VarOS:           runtime.GOOS,
		VarArch:         runtime.GOARCH,
		VarUser:         name,
		VarConfigDir:    config,
		VarCacheDir:     cache,
		VarCommanderDir: commander,
	}, nil
}

type rendered struct {
	file  *File
	vars  map[string]string
	order []Var
}

var refRe = regexp.MustCompile(`\{\{[^{}]*?\.([A-Za-z_][A-Za-z0-9_]*)`)

func references(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func render(f *File, engine, caller map[string]string) (*rendered, error) {
	base := map[string]string{}
	for k, v := range engine {
		base[k] = v
	}
	for k, v := range caller {
		base[k] = v
	}

	declared := map[string]bool{}
	for _, v := range f.Vars {
		declared[v.Name] = true
	}

	all := map[string]string{}
	for k, v := range base {
		all[k] = v
	}
	for _, v := range f.Vars {
		for _, ref := range references(v.Value) {
			if declared[ref] && ref != v.Name {
				return nil, fmt.Errorf(
					"vars.%s refers to vars.%s: a file's vars are rendered over the engine's and the caller's, never over each other",
					v.Name, ref)
			}
			if declared[ref] && ref == v.Name {
				return nil, fmt.Errorf("vars.%s refers to itself", v.Name)
			}
		}
		out, err := renderOne(v.Value, base, "vars."+v.Name)
		if err != nil {
			return nil, err
		}
		all[v.Name] = out
	}
	for k, v := range caller {
		all[k] = v
	}

	r := &rendered{file: &File{Version: f.Version, Name: f.Name, Desc: f.Desc, Vars: f.Vars}, vars: all}
	items, err := renderEntries(f.Items, all)
	if err != nil {
		return nil, err
	}
	r.file.Items = items
	r.order = varOrder(f, all, caller)
	return r, nil
}

func varOrder(f *File, all, caller map[string]string) []Var {
	var out []Var
	seen := map[string]bool{}
	for _, v := range f.Vars {
		out = append(out, Var{Name: v.Name, Value: all[v.Name]})
		seen[v.Name] = true
	}
	var extra []string
	for k := range caller {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, Var{Name: k, Value: all[k]})
		seen[k] = true
	}
	engine := map[string]bool{}
	for _, name := range EngineVars {
		engine[name] = true
	}
	for _, s := range templatesOf(f) {
		for _, ref := range references(s) {
			if seen[ref] || !engine[ref] {
				continue
			}
			out = append(out, Var{Name: ref, Value: all[ref]})
			seen[ref] = true
		}
	}
	return out
}

func templatesOf(f *File) []string {
	var out []string
	var walk func(items []Entry)
	walk = func(items []Entry) {
		for _, e := range items {
			if e.Block != nil {
				walk(e.Block.Items)
				continue
			}
			out = append(out, itemTemplates(e.Item)...)
		}
	}
	walk(f.Items)
	return out
}

func itemTemplates(it *Item) []string {
	out := []string{it.When, it.Check, it.Cmd}
	out = append(out, it.Cmds...)
	if it.File != nil {
		out = append(out, it.File.Path, it.File.Mode, it.File.Owner, it.File.Group, it.File.Content)
	}
	if it.Dir != nil {
		out = append(out, it.Dir.Path, it.Dir.Mode, it.Dir.Owner, it.Dir.Group)
	}
	return out
}

func renderEntries(items []Entry, vars map[string]string) ([]Entry, error) {
	out := make([]Entry, 0, len(items))
	for _, e := range items {
		if e.Block != nil {
			sub, err := renderEntries(e.Block.Items, vars)
			if err != nil {
				return nil, err
			}
			out = append(out, Entry{Block: &Block{Desc: e.Block.Desc, Items: sub}})
			continue
		}
		it, err := renderItem(e.Item, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Item: it})
	}
	return out, nil
}

func renderItem(it *Item, vars map[string]string) (*Item, error) {
	out := &Item{
		Name:      it.Name,
		Desc:      it.Desc,
		Platforms: it.Platforms,
		Timeout:   it.Timeout,
	}
	var err error
	if out.When, err = renderOne(it.When, vars, it.Name+".when"); err != nil {
		return nil, err
	}
	if out.Check, err = renderOne(it.Check, vars, it.Name+".check"); err != nil {
		return nil, err
	}
	if out.Cmd, err = renderOne(it.Cmd, vars, it.Name+".cmd"); err != nil {
		return nil, err
	}
	for i, c := range it.Cmds {
		s, err := renderOne(c, vars, fmt.Sprintf("%s.cmds[%d]", it.Name, i))
		if err != nil {
			return nil, err
		}
		out.Cmds = append(out.Cmds, s)
	}
	if it.File != nil {
		spec := &FileSpec{}
		for _, f := range []struct {
			key  string
			from string
			to   *string
		}{
			{"file.path", it.File.Path, &spec.Path},
			{"file.mode", it.File.Mode, &spec.Mode},
			{"file.owner", it.File.Owner, &spec.Owner},
			{"file.group", it.File.Group, &spec.Group},
			{"file.content", it.File.Content, &spec.Content},
		} {
			if *f.to, err = renderOne(f.from, vars, it.Name+"."+f.key); err != nil {
				return nil, err
			}
		}
		out.File = spec
	}
	if it.Dir != nil {
		spec := &DirSpec{}
		for _, f := range []struct {
			key  string
			from string
			to   *string
		}{
			{"dir.path", it.Dir.Path, &spec.Path},
			{"dir.mode", it.Dir.Mode, &spec.Mode},
			{"dir.owner", it.Dir.Owner, &spec.Owner},
			{"dir.group", it.Dir.Group, &spec.Group},
		} {
			if *f.to, err = renderOne(f.from, vars, it.Name+"."+f.key); err != nil {
				return nil, err
			}
		}
		out.Dir = spec
	}
	return out, nil
}

func renderOne(text string, vars map[string]string, where string) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	t, err := template.New(where).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("%s: %w", where, missingVar(err, vars))
	}
	return b.String(), nil
}

var missingRe = regexp.MustCompile(`map has no entry for key "([^"]+)"`)

func missingVar(err error, vars map[string]string) error {
	m := missingRe.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	return fmt.Errorf("no var named %q (this run has %s)", m[1], strings.Join(names, ", "))
}
