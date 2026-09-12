package vault

import (
	"sort"
	"time"
)

type Bundle struct {
	App string `json:"app"`
	Env string `json:"env"`

	Values map[string]string `json:"values"`

	Sources map[string]Scope `json:"sources,omitempty"`

	Fingerprint string `json:"fingerprint,omitempty"`

	Machine string `json:"machine,omitempty"`

	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

func (b *Bundle) Names() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Values))
	for name := range b.Values {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (b *Bundle) Redacted() *Bundle {
	if b == nil {
		return nil
	}
	out := *b
	out.Values = make(map[string]string, len(b.Values))
	for name := range b.Values {
		out.Values[name] = Redacted
	}
	return &out
}
