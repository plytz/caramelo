package vault

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvFileLifecycle(t *testing.T) {
	dir := SecretsDir(t.TempDir())
	f, err := WriteEnvFile(dir, "caramelo-shop-production-web-1", map[string]string{
		"DB_PASSWORD": "chosen",
		"API_KEY":     "k",
	})
	if err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	if f.Path == "" {
		t.Fatal("nothing was written")
	}
	if got := filepath.Dir(f.Path); got != dir {
		t.Errorf("the file is at %s, want it under %s", f.Path, dir)
	}
	if !strings.HasPrefix(filepath.Base(f.Path), "caramelo-shop-production-web-1-") {
		t.Errorf("the name %q does not say what it is for", filepath.Base(f.Path))
	}
	st, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != SecretsFileMode {
		t.Errorf("the file is mode %04o, want %04o", st.Mode().Perm(), SecretsFileMode)
	}
	if dst, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if dst.Mode().Perm() != SecretsDirMode {
		t.Errorf("the directory is mode %04o, want %04o", dst.Mode().Perm(), SecretsDirMode)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) != "API_KEY=k\nDB_PASSWORD=chosen\n" {
		t.Errorf("the file holds %q", raw)
	}
	if !reflect.DeepEqual(f.Names, []string{"API_KEY", "DB_PASSWORD"}) {
		t.Errorf("Names = %v", f.Names)
	}
	path := f.Path
	if err := f.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the file survived Remove: %v", err)
	}

	if err := f.Remove(); err != nil {
		t.Errorf("the second Remove: %v", err)
	}
	if err := (&EnvFile{}).Remove(); err != nil {
		t.Errorf("Remove of nothing: %v", err)
	}
	var nilFile *EnvFile
	if err := nilFile.Remove(); err != nil {
		t.Errorf("Remove of nil: %v", err)
	}
}

func TestWriteEnvFileWithNothingToWrite(t *testing.T) {
	dir := SecretsDir(t.TempDir())
	f, err := WriteEnvFile(dir, "web", nil)
	if err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	if f.Path != "" {
		t.Errorf("a file was written for no secrets: %s", f.Path)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory was created for nothing: %v", err)
	}
}

func TestEnvFileRefusesANewline(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "-----BEGIN KEY-----\nabc\n"} {
		if _, err := EnvFileContent(map[string]string{"KEY": value}); err == nil {
			t.Errorf("a value containing a newline was accepted: %q", value)
		} else if !strings.Contains(err.Error(), "base64") {
			t.Errorf("the error does not say what to do instead: %v", err)
		}
	}

	content, err := EnvFileContent(map[string]string{"K": `a b"c'd=e#f$g`})
	if err != nil {
		t.Fatal(err)
	}
	if content != "K=a b\"c'd=e#f$g\n" {
		t.Errorf("content = %q", content)
	}
}

func TestParseDotenv(t *testing.T) {
	in := `# a comment
DB_PASSWORD=chosen

export API_KEY=from-a-sourced-file
QUOTED="with spaces and a \"quote\" and a \n newline"
LITERAL='$not ${expanded} \n at all'
EMPTY=
HASH=pa#ssword
EQUALS=a=b=c
SPACED =  trimmed
`
	got, err := ParseDotenv(in)
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	want := map[string]string{
		"DB_PASSWORD": "chosen",
		"API_KEY":     "from-a-sourced-file",
		"QUOTED":      "with spaces and a \"quote\" and a \n newline",
		"LITERAL":     `$not ${expanded} \n at all`,
		"EMPTY":       "",
		"HASH":        "pa#ssword",
		"EQUALS":      "a=b=c",
		"SPACED":      "trimmed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDotenv =\n%#v\nwant\n%#v", got, want)
	}
}

func TestParseDotenvErrors(t *testing.T) {
	for what, in := range map[string]string{
		"no equals":     "DB_PASSWORD chosen\n",
		"lower case":    "db_password=chosen\n",
		"a dash":        "DB-PASSWORD=chosen\n",
		"a digit first": "2FA=x\n",
		"set twice":     "K=a\nK=b\n",
		"bad escape":    `K="a \q b"` + "\n",
	} {
		if _, err := ParseDotenv(in); err == nil {
			t.Errorf("%s was accepted", what)
		} else if !strings.HasPrefix(err.Error(), "line ") {
			t.Errorf("the error for %s does not name the line: %v", what, err)
		}
	}

	long := strings.Repeat("SECRETVALUE", 20)
	_, err := ParseDotenv(long + "\n")
	if err == nil {
		t.Fatal("a line with no equals was accepted")
	}
	if strings.Contains(err.Error(), long) {
		t.Errorf("the error quotes the whole line: %v", err)
	}
}

func TestReadDotenvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.production")
	if err := os.WriteFile(path, []byte("DB_PASSWORD=chosen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDotenvFile(path)
	if err != nil {
		t.Fatalf("ReadDotenvFile: %v", err)
	}
	if got["DB_PASSWORD"] != "chosen" {
		t.Errorf("got %v", got)
	}
	if _, err := ReadDotenvFile(filepath.Join(dir, "nothing")); err == nil {
		t.Error("a missing file was accepted")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDotenvFile(empty); err == nil {
		t.Error("a file with no pairs was accepted")
	}
}

func TestRender(t *testing.T) {
	values := map[string]string{
		"SIMPLE": "plain",
		"SPACED": "two words",
		"QUOTED": "it's",
		"EMPTY":  "",
	}
	got, err := Render(values, FormatEnv)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "EMPTY=''\n" +
		"QUOTED='it'\\''s'\n" +
		"SIMPLE=plain\n" +
		"SPACED='two words'\n"
	if got != want {
		t.Errorf("Render(env) =\n%q\nwant\n%q", got, want)
	}

	if bare, err := Render(values, ""); err != nil || bare != want {
		t.Errorf("Render(\"\") = %q, %v", bare, err)
	}
	j, err := Render(map[string]string{"A": "1", "B": `say "hi"`}, FormatJSON)
	if err != nil {
		t.Fatalf("Render(json): %v", err)
	}
	if j != "{\n  \"A\": \"1\",\n  \"B\": \"say \\\"hi\\\"\"\n}\n" {
		t.Errorf("Render(json) = %q", j)
	}
	if _, err := Render(values, "yaml"); err == nil {
		t.Error("an unknown format was accepted")
	}

	if _, err := Render(map[string]string{"TLS_KEY": "a\nb"}, FormatEnv); err == nil {
		t.Error("a value with a newline was written as a KEY=value line")
	}
	if _, err := Render(map[string]string{"TLS_KEY": "a\nb"}, FormatJSON); err != nil {
		t.Errorf("the json form refused a newline: %v", err)
	}
}

func TestPasswordSecret(t *testing.T) {
	for dep, want := range map[string]string{
		"db": "DB_PASSWORD", "cache": "CACHE_PASSWORD",
		"my-db": "MY_DB_PASSWORD", " db ": "DB_PASSWORD",
	} {
		if got := PasswordSecret(dep); got != want {
			t.Errorf("PasswordSecret(%q) = %q, want %q", dep, got, want)
		}
		if err := ValidateName(want); err != nil {
			t.Errorf("%q is not a legal secret name: %v", want, err)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if seen[pw] {
			t.Fatal("GeneratePassword repeated itself")
		}
		seen[pw] = true
		if len(pw) != 32 {
			t.Errorf("the password is %d characters: %q", len(pw), pw)
		}
		if strings.ContainsAny(pw, "+/=") {
			t.Errorf("the password %q holds a character that changes a URL's meaning", pw)
		}
		if pw == DevPassword {
			t.Error("the generated password is the development one")
		}
		if err := ValidateValue("K", pw); err != nil {
			t.Errorf("the generated password is not one this vault holds: %v", err)
		}
	}
}
