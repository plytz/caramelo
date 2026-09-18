package task

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed files/*.yaml
var files embed.FS

const filesDir = "files"

func Tasks() []string {
	entries, err := fs.ReadDir(files, filesDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), path.Ext(e.Name())))
	}
	sort.Strings(out)
	return out
}

func Source(name string) ([]byte, error) {
	b, err := files.ReadFile(path.Join(filesDir, name+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("no task called %q (this binary carries %s)", name, strings.Join(Tasks(), ", "))
	}
	return b, nil
}

func Load(name string) (*File, error) {
	b, err := Source(name)
	if err != nil {
		return nil, err
	}
	f, err := Parse(b, name+".yaml")
	if err != nil {
		return nil, err
	}
	if f.Name != name {
		return nil, fmt.Errorf("%s.yaml calls itself %q: a task file is named after the task", name, f.Name)
	}
	return f, nil
}

type Info struct {
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
}

func List() ([]Info, error) {
	out := []Info{}
	for _, name := range Tasks() {
		f, err := Load(name)
		if err != nil {
			return nil, err
		}
		out = append(out, Info{Name: f.Name, Desc: f.Desc})
	}
	return out, nil
}
