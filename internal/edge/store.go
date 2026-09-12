package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const routesFileMode fs.FileMode = 0o600

const routesDirMode fs.FileMode = 0o700

func LoadTable(path string) (Table, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Table{}, nil
	}
	if err != nil {
		return Table{}, fmt.Errorf("edge: read %s: %w", path, err)
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return Table{}, fmt.Errorf("edge: %s: %w", path, err)
	}
	for i := range t.Routes {
		t.Routes[i].Host = NormalizeHost(t.Routes[i].Host)

		for j := range t.Routes[i].Targets {
			t.Routes[i].Targets[j].Inflight = 0
		}
	}
	if err := t.Validate(); err != nil {
		return Table{}, fmt.Errorf("edge: %s: %w", path, err)
	}
	return t, nil
}

func SaveTable(path string, t Table) error {
	if err := os.MkdirAll(filepath.Dir(path), routesDirMode); err != nil {
		return fmt.Errorf("edge: %s: %w", filepath.Dir(path), err)
	}
	clean := t.Clone()
	for i := range clean.Routes {
		for j := range clean.Routes[i].Targets {
			clean.Routes[i].Targets[j].Inflight = 0
		}
	}
	b, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return fmt.Errorf("edge: route table: %w", err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".routes-*.json")
	if err != nil {
		return fmt.Errorf("edge: %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(routesFileMode); err != nil {
		f.Close()
		return fmt.Errorf("edge: %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("edge: %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("edge: %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("edge: %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("edge: %s: %w", path, err)
	}
	return nil
}
