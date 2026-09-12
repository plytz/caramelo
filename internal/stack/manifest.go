package stack

import (
	"fmt"
	"os"
	"path/filepath"
)

var manifests = map[string][]string{

	Go: {"go.mod", "go.sum"},

	Node: {
		"package.json", "package-lock.json", "npm-shrinkwrap.json",
		"pnpm-lock.yaml", "pnpm-workspace.yaml", "yarn.lock",
		".npmrc", ".yarnrc.yml",
	},

	Python: {"requirements.txt", "constraints.txt", "pyproject.toml", "poetry.lock"},

	Rails: {"Gemfile", "Gemfile.lock", ".ruby-version"},

	Docker: nil,
}

type Manifest struct {
	Files []string `json:"files,omitempty"`

	NeedsSource bool `json:"needs_source,omitempty"`
}

func (m Manifest) Empty() bool { return len(m.Files) == 0 }

func Manifests(stackName string) []string {
	return append([]string(nil), manifests[stackName]...)
}

func ManifestsIn(stackName, dir string) (Manifest, error) {
	var out Manifest
	for _, name := range manifests[stackName] {
		if isFile(dir, name) {
			out.Files = append(out.Files, name)
		}
	}
	needs, err := needsSource(stackName, dir, out.Files)
	if err != nil {
		return Manifest{}, err
	}
	out.NeedsSource = needs
	return out, nil
}

func needsSource(stackName, dir string, found []string) (bool, error) {
	switch stackName {
	case Python:
		for _, f := range found {
			if f == "requirements.txt" {
				return false, nil
			}
		}

		return len(found) > 0, nil
	case Rails:
		gems, err := filepath.Glob(filepath.Join(dir, "*.gemspec"))
		if err != nil {
			return false, fmt.Errorf("look for a gemspec in %s: %w", dir, err)
		}
		return len(gems) > 0, nil
	}
	return false, nil
}

func isFile(dir, name string) bool {
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
