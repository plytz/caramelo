package task

import (
	"strings"
	"testing"
)

func engineVarsFixture() map[string]string {
	return map[string]string{
		VarHostname:     "laptop",
		VarOS:           "linux",
		VarArch:         "arm64",
		VarUser:         "alex",
		VarConfigDir:    fixtureHome + "/.config",
		VarCacheDir:     fixtureHome + "/.cache",
		VarCommanderDir: fixtureDir,
	}
}

func parseFixture(t *testing.T, body string) *File {
	t.Helper()
	f, err := Parse([]byte(body), "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVarsComeFromTheFileTheCallerAndTheEngine(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
vars:
  dir: '{{ .CommanderDir }}'
  name: '{{ .Hostname }}'
items:
  - name: one
    check: 'test -d "{{ .dir }}" && echo {{ .name }} {{ .OS }}'
`)
	r, err := render(f, engineVarsFixture(), map[string]string{"name": "desk"})
	if err != nil {
		t.Fatal(err)
	}
	want := `test -d "` + fixtureDir + `" && echo desk linux`
	if got := r.file.Items[0].Item.Check; got != want {
		t.Errorf("check = %q, want %q", got, want)
	}
}

func TestAFileVarNeverReadsAnotherFileVar(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
vars:
  dir: '{{ .CommanderDir }}'
  vpn: '{{ .dir }}/vpn'
items:
  - name: one
    check: 'test -d "{{ .vpn }}"'
`)
	_, err := render(f, engineVarsFixture(), nil)
	if err == nil {
		t.Fatal("rendered a file whose var reads another var")
	}
	for _, want := range []string{"vars.vpn", "vars.dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %s", err, want)
		}
	}
}

func TestAMissingVarIsNamedBeforeAnythingRuns(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
items:
  - name: one
    check: 'test -d "{{ .nowhere }}"'
`)
	_, err := render(f, engineVarsFixture(), nil)
	if err == nil {
		t.Fatal("rendered a file naming a var nothing defines")
	}
	if !strings.Contains(err.Error(), `no var named "nowhere"`) {
		t.Errorf("err = %v, want it to name the var", err)
	}
	if !strings.Contains(err.Error(), "one.check") {
		t.Errorf("err = %v, want it to name the item and the key", err)
	}
}

func TestATemplateThatDoesNotParseNamesTheItemAndTheKey(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
items:
  - name: one
    cmd: 'echo {{ .OS'
`)
	_, err := render(f, engineVarsFixture(), nil)
	if err == nil {
		t.Fatal("rendered a template that does not parse")
	}
	if !strings.Contains(err.Error(), "one.cmd") {
		t.Errorf("err = %v, want it to name the item and the key", err)
	}
}

func TestTheVarsOfARunAreListedFileVarsFirst(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
vars:
  dir: '{{ .CommanderDir }}'
items:
  - name: one
    check: 'test -d "{{ .dir }}/{{ .CacheDir }}" && echo {{ .OS }}'
`)
	r, err := render(f, engineVarsFixture(), map[string]string{"extra": "1"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range r.order {
		names = append(names, v.Name)
	}
	want := []string{"dir", "extra", VarCacheDir, VarOS}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("vars = %v, want %v", names, want)
	}
}

func TestEveryTemplateOfAFileIsRenderedOnce(t *testing.T) {
	f := parseFixture(t, `version: 1
name: x
vars:
  dir: '{{ .CommanderDir }}'
items:
  - parallel:
      items:
        - name: a
          dir:
            path: '{{ .dir }}/a'
            mode: '0700'
        - name: b
          file:
            path: '{{ .dir }}/b'
            content: '{{ .Hostname }}'
  - name: c
    when: 'test {{ .OS }} = linux'
    cmds:
      - 'echo {{ .Arch }}'
`)
	r, err := render(f, engineVarsFixture(), nil)
	if err != nil {
		t.Fatal(err)
	}
	block := r.file.Items[0].Block
	if got := block.Items[0].Item.Dir.Path; got != fixtureDir+"/a" {
		t.Errorf("dir.path = %q", got)
	}
	if got := block.Items[1].Item.File.Content; got != "laptop" {
		t.Errorf("file.content = %q", got)
	}
	if got := r.file.Items[1].Item.When; got != "test linux = linux" {
		t.Errorf("when = %q", got)
	}
	if got := r.file.Items[1].Item.Cmds[0]; got != "echo arm64" {
		t.Errorf("cmds[0] = %q", got)
	}
}
