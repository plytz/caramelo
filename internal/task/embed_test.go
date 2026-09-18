package task

import (
	"sort"
	"strings"
	"testing"
)

func TestEveryEmbeddedTaskParses(t *testing.T) {
	names := Tasks()
	if len(names) == 0 {
		t.Fatal("this binary carries no tasks")
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("tasks = %v, want them sorted", names)
	}
	for _, name := range names {
		f, err := Load(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.TrimSpace(f.Desc) == "" {
			t.Errorf("%s says nothing about what it does", name)
		}
		walkItems(f.Items, func(it *Item) {
			if strings.TrimSpace(it.Desc) == "" {
				t.Errorf("%s: %s says nothing about what it does", name, it.Name)
			}
			if !it.builtin() && it.Check == "" {
				t.Errorf("%s: %s has a command and no check: it would change the machine every run", name, it.Name)
			}
		})
	}
}

func TestNoEmbeddedTaskCarriesAComment(t *testing.T) {
	for _, name := range Tasks() {
		b, err := Source(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				t.Errorf("%s.yaml:%d is a comment; what it says belongs in a desc", name, i+1)
			}
		}
	}
}

func TestAnUnknownTaskNamesTheOnesThereAre(t *testing.T) {
	_, err := Source("nosuchtask")
	if err == nil {
		t.Fatal("loaded a task that is not there")
	}
	for _, want := range append([]string{"nosuchtask"}, Tasks()...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestListCarriesEveryTaskWithItsDescription(t *testing.T) {
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(Tasks()) {
		t.Fatalf("list = %+v, want one row per task", list)
	}
	for _, info := range list {
		if info.Name == "" || info.Desc == "" {
			t.Errorf("row = %+v", info)
		}
	}
}

func TestCommanderSetupIsTheTaskCommanderInitRuns(t *testing.T) {
	f, err := Load("commander-setup")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	walkItems(f.Items, func(it *Item) { names = append(names, it.Name) })
	want := []string{"config-dir", "vpn-dir", "cache-dir", "config", "identity"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("items = %v, want %v", names, want)
	}
	if f.Items[0].Block == nil {
		t.Error("the three directories are not a parallel block")
	}
}
