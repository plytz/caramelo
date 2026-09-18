package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	topLevelKeys = []string{"version", "name", "desc", "vars", "items"}
	itemKeys     = []string{"name", "desc", "check", "cmd", "cmds", "when", "platforms", "timeout", "file", "dir"}
	blockKeys    = []string{"desc", "items"}
	fileKeys     = []string{"path", "mode", "owner", "group", "content"}
	dirKeys      = []string{"path", "mode", "owner", "group"}
)

var modeRe = regexp.MustCompile(`^[0-7]{3,4}$`)

func Parse(data []byte, path string) (*File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nodeErr(path, nil, "%v", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nodeErr(path, nil, "the file is empty: want version, name and items")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nodeErr(path, root, "must be a mapping of %s", quoteList(topLevelKeys))
	}
	f := &File{}
	sawVersion, sawItems := false, false
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		var err error
		switch k.Value {
		case "version":
			sawVersion = true
			var n int
			if n, err = scalarInt(path, v, "version"); err == nil && n != Version {
				err = nodeErr(path, v, "version %d: this runner reads version %d task files", n, Version)
			}
			f.Version = n
		case "name":
			f.Name, err = scalarString(path, v, "name")
		case "desc":
			f.Desc, err = scalarString(path, v, "desc")
		case "vars":
			f.Vars, err = parseVars(path, v)
		case "items":
			sawItems = true
			f.Items, err = parseEntries(path, v)
		default:
			err = nodeErr(path, k, "unknown key %q (known keys: %s)", k.Value, quoteList(topLevelKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if !sawVersion {
		return nil, nodeErr(path, root, "no version: a task file says %q", "version: 1")
	}
	if strings.TrimSpace(f.Name) == "" {
		return nil, nodeErr(path, root, "no name: a task file says what it is called")
	}
	if !sawItems || len(f.Items) == 0 {
		return nil, nodeErr(path, root, "no items: a task file lists what it does under %q", "items")
	}
	if err := uniqueNames(path, f.Items, map[string]bool{}); err != nil {
		return nil, err
	}
	return f, nil
}

func parseVars(path string, node *yaml.Node) ([]Var, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nodeErr(path, node, "vars must be a mapping of name to value")
	}
	var out []Var
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if seen[k.Value] {
			return nil, nodeErr(path, k, "vars: %q is declared twice", k.Value)
		}
		seen[k.Value] = true
		s, err := scalarString(path, v, "vars."+k.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, Var{Name: k.Value, Value: s})
	}
	return out, nil
}

func parseEntries(path string, node *yaml.Node) ([]Entry, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, nodeErr(path, node, "items must be a list of items")
	}
	if len(node.Content) == 0 {
		return nil, nodeErr(path, node, "items is empty: a list with nothing in it does nothing")
	}
	out := make([]Entry, 0, len(node.Content))
	for _, n := range node.Content {
		e, err := parseEntry(path, n)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseEntry(path string, node *yaml.Node) (Entry, error) {
	if node.Kind != yaml.MappingNode {
		return Entry{}, nodeErr(path, node, "an item must be a mapping of %s", quoteList(itemKeys))
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "parallel" {
			continue
		}
		if len(node.Content) != 2 {
			return Entry{}, nodeErr(path, node, "parallel: stands alone in its item, beside no other key")
		}
		b, err := parseBlock(path, node.Content[i+1])
		if err != nil {
			return Entry{}, err
		}
		return Entry{Block: b}, nil
	}
	it, err := parseItem(path, node)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Item: it}, nil
}

func parseBlock(path string, node *yaml.Node) (*Block, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nodeErr(path, node, "parallel must be a mapping of %s", quoteList(blockKeys))
	}
	b := &Block{}
	sawItems := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		var err error
		switch k.Value {
		case "desc":
			b.Desc, err = scalarString(path, v, "parallel.desc")
		case "items":
			sawItems = true
			b.Items, err = parseEntries(path, v)
		default:
			err = nodeErr(path, k, "parallel: unknown key %q (known keys: %s)", k.Value, quoteList(blockKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if !sawItems {
		return nil, nodeErr(path, node, "parallel: no items: a parallel block lists what runs together")
	}
	return b, nil
}

func parseItem(path string, node *yaml.Node) (*Item, error) {
	it := &Item{}
	sawCmd, sawCmds := false, false
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		var err error
		switch k.Value {
		case "name":
			it.Name, err = scalarString(path, v, "name")
		case "desc":
			it.Desc, err = scalarString(path, v, "desc")
		case "check":
			it.Check, err = scalarString(path, v, "check")
		case "cmd":
			sawCmd = true
			it.Cmd, err = scalarString(path, v, "cmd")
		case "cmds":
			sawCmds = true
			it.Cmds, err = scalarList(path, v, "cmds")
		case "when":
			it.When, err = scalarString(path, v, "when")
		case "platforms":
			it.Platforms, err = scalarList(path, v, "platforms")
		case "timeout":
			it.Timeout, err = parseTimeout(path, v)
		case "file":
			it.File, err = parseFileSpec(path, v)
		case "dir":
			it.Dir, err = parseDirSpec(path, v)
		default:
			err = nodeErr(path, k, "unknown key %q (known keys: %s)", k.Value, quoteList(itemKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(it.Name) == "" {
		return nil, nodeErr(path, node, "an item needs a name: it is what the run reports and what a person reads")
	}
	if sawCmd && sawCmds {
		return nil, nodeErr(path, node, "%s: cmd and cmds together: one command or a list of them, not both", it.Name)
	}
	if it.File != nil && it.Dir != nil {
		return nil, nodeErr(path, node, "%s: file: and dir: together: an item changes one thing", it.Name)
	}
	builtin := it.builtin()
	script := it.Check != "" || sawCmd || sawCmds
	switch {
	case builtin && script:
		return nil, nodeErr(path, node,
			"%s: a built-in beside check or cmd: a file: or dir: item checks and changes itself", it.Name)
	case !builtin && !script:
		return nil, nodeErr(path, node,
			"%s: nothing to do: an item needs a check, a cmd, or a file: or dir: of its own", it.Name)
	}
	return it, nil
}

func parseFileSpec(path string, node *yaml.Node) (*FileSpec, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nodeErr(path, node, "file must be a mapping of %s", quoteList(fileKeys))
	}
	spec := &FileSpec{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		var err error
		switch k.Value {
		case "path":
			spec.Path, err = scalarString(path, v, "file.path")
		case "mode":
			spec.Mode, err = parseMode(path, v, "file.mode")
		case "owner":
			spec.Owner, err = scalarString(path, v, "file.owner")
		case "group":
			spec.Group, err = scalarString(path, v, "file.group")
		case "content":
			spec.Content, err = scalarString(path, v, "file.content")
		default:
			err = nodeErr(path, k, "file: unknown key %q (known keys: %s)", k.Value, quoteList(fileKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(spec.Path) == "" {
		return nil, nodeErr(path, node, "file: no path: a file item says which file it writes")
	}
	return spec, nil
}

func parseDirSpec(path string, node *yaml.Node) (*DirSpec, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nodeErr(path, node, "dir must be a mapping of %s", quoteList(dirKeys))
	}
	spec := &DirSpec{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		var err error
		switch k.Value {
		case "path":
			spec.Path, err = scalarString(path, v, "dir.path")
		case "mode":
			spec.Mode, err = parseMode(path, v, "dir.mode")
		case "owner":
			spec.Owner, err = scalarString(path, v, "dir.owner")
		case "group":
			spec.Group, err = scalarString(path, v, "dir.group")
		default:
			err = nodeErr(path, k, "dir: unknown key %q (known keys: %s)", k.Value, quoteList(dirKeys))
		}
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(spec.Path) == "" {
		return nil, nodeErr(path, node, "dir: no path: a dir item says which directory it makes")
	}
	return spec, nil
}

func parseMode(path string, node *yaml.Node, key string) (string, error) {
	s, err := scalarString(path, node, key)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", nil
	}
	if !modeRe.MatchString(s) {
		return "", nodeErr(path, node, "%s %q: a mode is three or four octal digits, e.g. 0700", key, s)
	}
	if len(s) == 4 && s[0] != '0' {
		return "", nodeErr(path, node,
			"%s %q: a task's mode is permissions only, and the leading %q asks for a setuid, setgid or sticky bit",
			key, s, s[:1])
	}
	return s, nil
}

func parseTimeout(path string, node *yaml.Node) (time.Duration, error) {
	s, err := scalarString(path, node, "timeout")
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, nodeErr(path, node, "timeout %q: want a duration such as 30s or 5m", s)
	}
	if d <= 0 {
		return 0, nodeErr(path, node, "timeout %q: want a duration greater than zero", s)
	}
	return d, nil
}

func uniqueNames(path string, items []Entry, seen map[string]bool) error {
	for _, e := range items {
		if e.Block != nil {
			if err := uniqueNames(path, e.Block.Items, seen); err != nil {
				return err
			}
			continue
		}
		if seen[e.Item.Name] {
			return nodeErr(path, nil, "two items are called %q: a name says which one a report means", e.Item.Name)
		}
		seen[e.Item.Name] = true
	}
	return nil
}

func scalarString(path string, node *yaml.Node, key string) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", nodeErr(path, node, "%s must be a string", key)
	}
	return node.Value, nil
}

func scalarInt(path string, node *yaml.Node, key string) (int, error) {
	s, err := scalarString(path, node, key)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, nodeErr(path, node, "%s must be a number, got %q", key, s)
	}
	return n, nil
}

func scalarList(path string, node *yaml.Node, key string) ([]string, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, nodeErr(path, node, "%s must be a list", key)
	}
	out := make([]string, 0, len(node.Content))
	for _, n := range node.Content {
		s, err := scalarString(path, n, key)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, nodeErr(path, node, "%s is empty", key)
	}
	return out, nil
}

func nodeErr(path string, node *yaml.Node, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	where := path
	if node != nil && node.Line > 0 {
		if where == "" {
			where = fmt.Sprintf("line %d", node.Line)
		} else {
			where = fmt.Sprintf("%s:%d", where, node.Line)
		}
	}
	if where == "" {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %s", where, msg)
}

func quoteList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return strings.Join(out, ", ")
}
