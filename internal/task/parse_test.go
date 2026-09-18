package task

import (
	"strings"
	"testing"
	"time"
)

const wholeFile = `version: 1
name: everything
desc: one of every key
vars:
  dir: /tmp/x
items:
  - parallel:
      desc: together
      items:
        - name: a-dir
          desc: a directory
          dir:
            path: '{{ .dir }}/a'
            mode: '0700'
            owner: root
            group: root
        - name: a-file
          file:
            path: '{{ .dir }}/a/f'
            mode: '0600'
            content: hello
  - name: one
    desc: a check and a command
    when: test -d /tmp
    platforms: [linux, darwin]
    timeout: 30s
    check: test -f /tmp/x
    cmd: touch /tmp/x
  - name: many
    check: false
    cmds:
      - echo one
      - echo two
`

func TestParseReadsEveryKey(t *testing.T) {
	f, err := Parse([]byte(wholeFile), "everything.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 || f.Name != "everything" || f.Desc != "one of every key" {
		t.Errorf("file = %+v", f)
	}
	if len(f.Vars) != 1 || f.Vars[0].Name != "dir" || f.Vars[0].Value != "/tmp/x" {
		t.Errorf("vars = %+v", f.Vars)
	}
	if len(f.Items) != 3 || f.Items[0].Block == nil {
		t.Fatalf("items = %+v", f.Items)
	}
	block := f.Items[0].Block
	if block.Desc != "together" || len(block.Items) != 2 {
		t.Errorf("block = %+v", block)
	}
	if d := block.Items[0].Item.Dir; d == nil || d.Path != "{{ .dir }}/a" || d.Mode != "0700" || d.Owner != "root" {
		t.Errorf("dir = %+v", d)
	}
	if fs := block.Items[1].Item.File; fs == nil || fs.Content != "hello" || fs.Mode != "0600" {
		t.Errorf("file = %+v", fs)
	}
	one := f.Items[1].Item
	if one.When != "test -d /tmp" || one.Timeout != 30*time.Second || len(one.Platforms) != 2 {
		t.Errorf("item = %+v", one)
	}
	if got := f.Items[2].Item.Cmds; len(got) != 2 || got[1] != "echo two" {
		t.Errorf("cmds = %v", got)
	}
}

func TestParseRefusesWhatIsNotATaskFile(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a taskfile", "version: '3'\ntasks:\n  build:\n    cmds: [go build]\n", "this runner reads version 1"},
		{"no version", "name: x\nitems:\n  - name: a\n    cmd: true\n", "no version"},
		{"empty", "", "the file is empty"},
		{"no name", "version: 1\nitems:\n  - name: a\n    cmd: true\n", "no name"},
		{"no items", "version: 1\nname: x\n", "no items"},
		{"empty items", "version: 1\nname: x\nitems: []\n", "items is empty"},
		{"unknown top key", "version: 1\nname: x\nsteps: []\n", `unknown key "steps"`},
		{"unknown item key", "version: 1\nname: x\nitems:\n  - name: a\n    shell: bash\n", `unknown key "shell"`},
		{"unknown block key", "version: 1\nname: x\nitems:\n  - parallel:\n      workers: 4\n      items: []\n", `unknown key "workers"`},
		{"no item name", "version: 1\nname: x\nitems:\n  - cmd: true\n", "an item needs a name"},
		{"two items one name", `version: 1
name: x
items:
  - parallel:
      items:
        - name: a
          cmd: true
  - name: a
    cmd: true
`, `two items are called "a"`},
		{"nothing to do", "version: 1\nname: x\nitems:\n  - name: a\n    desc: nothing\n", "nothing to do"},
		{"builtin beside check", "version: 1\nname: x\nitems:\n  - name: a\n    check: true\n    dir:\n      path: /tmp/a\n",
			"a built-in beside check or cmd"},
		{"cmd and cmds", "version: 1\nname: x\nitems:\n  - name: a\n    cmd: true\n    cmds: [true]\n", "cmd and cmds together"},
		{"parallel with no items", "version: 1\nname: x\nitems:\n  - parallel:\n      desc: nothing\n", "parallel: no items"},
		{"parallel beside a check", "version: 1\nname: x\nitems:\n  - parallel:\n      items:\n        - name: a\n          cmd: true\n    check: true\n",
			"parallel: stands alone"},
		{"file with no path", "version: 1\nname: x\nitems:\n  - name: a\n    file:\n      content: hi\n", "file: no path"},
		{"dir with no path", "version: 1\nname: x\nitems:\n  - name: a\n    dir:\n      mode: '0700'\n", "dir: no path"},
		{"a mode that is not octal", "version: 1\nname: x\nitems:\n  - name: a\n    dir:\n      path: /tmp/a\n      mode: rwx\n",
			"three or four octal digits"},
		{"a mode with an eight", "version: 1\nname: x\nitems:\n  - name: a\n    dir:\n      path: /tmp/a\n      mode: '0780'\n",
			"three or four octal digits"},
		{"a mode asking for a sticky bit", "version: 1\nname: x\nitems:\n  - name: a\n    dir:\n      path: /tmp/a\n      mode: '1777'\n",
			"setuid, setgid or sticky"},
		{"a timeout that is no duration", "version: 1\nname: x\nitems:\n  - name: a\n    cmd: true\n    timeout: soon\n",
			"want a duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in), "x.yaml")
			if err == nil {
				t.Fatalf("parsed %q, want a refusal naming %q", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "x.yaml") {
				t.Errorf("err = %v, want it to name the file", err)
			}
		})
	}
}

func TestARefusalNamesTheLine(t *testing.T) {
	_, err := Parse([]byte("version: 1\nname: x\nitems:\n  - name: a\n    shell: bash\n"), "x.yaml")
	if err == nil {
		t.Fatal("parsed a file with an unknown key")
	}
	if !strings.Contains(err.Error(), "x.yaml:5") {
		t.Errorf("err = %v, want it to name line 5", err)
	}
}
