package release

import (
	"sort"
	"time"
)

type Image struct {
	ReleaseID int64 `json:"release_id"`

	Service string `json:"service"`
	Ref     string `json:"ref,omitempty"`

	Arch string `json:"arch"`

	Machine string `json:"machine"`

	ID string `json:"id,omitempty"`

	BuiltAt time.Time `json:"built_at,omitempty"`
}

type Images []Image

func (l Images) Sort() {
	sort.Slice(l, func(i, j int) bool {
		a, b := l[i], l[j]
		switch {
		case a.Arch != b.Arch:
			return a.Arch < b.Arch
		case a.Service != b.Service:
			return a.Service < b.Service
		}
		return a.Machine < b.Machine
	})
}

func (l Images) ForArch(arch string) Images {
	out := make(Images, 0, len(l))
	for _, im := range l {
		if im.Arch == arch {
			out = append(out, im)
		}
	}
	return out
}

func (l Images) Machines(arch string, services []string) []string {
	if len(services) == 0 {
		return nil
	}
	have := map[string]map[string]bool{}
	for _, im := range l.ForArch(arch) {
		if have[im.Machine] == nil {
			have[im.Machine] = map[string]bool{}
		}
		have[im.Machine][im.Service] = true
	}
	var out []string
	for m, set := range have {
		complete := true
		for _, s := range services {
			if !set[s] {
				complete = false
				break
			}
		}
		if complete {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func (l Images) Arches() []string {
	seen := map[string]bool{}
	var out []string
	for _, im := range l {
		if im.Arch == "" || seen[im.Arch] {
			continue
		}
		seen[im.Arch] = true
		out = append(out, im.Arch)
	}
	sort.Strings(out)
	return out
}
