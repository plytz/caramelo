package remote

import "testing"

func TestSplitWords(t *testing.T) {
	cases := map[string][]string{
		"":                              nil,
		"-F /tmp/cfg -i /tmp/key":       {"-F", "/tmp/cfg", "-i", "/tmp/key"},
		`-o 'ProxyCommand=nc %h %p'`:    {"-o", "ProxyCommand=nc %h %p"},
		`-o "IdentityFile=/a b/key" -v`: {"-o", "IdentityFile=/a b/key", "-v"},
		`a\ b c`:                        {"a b", "c"},
		"  spaced   out  ":              {"spaced", "out"},
	}
	for in, want := range cases {
		got, err := SplitWords(in)
		if err != nil {
			t.Errorf("SplitWords(%q) error: %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("SplitWords(%q) = %q, want %q", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("SplitWords(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
	for _, bad := range []string{`"open`, `'open`, `trailing\`} {
		if _, err := SplitWords(bad); err == nil {
			t.Errorf("SplitWords(%q) accepted malformed input", bad)
		}
	}
}
