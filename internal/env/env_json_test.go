package env

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnEnvNeverPushedCarriesNoPushedAt(t *testing.T) {
	b, err := json.Marshal(Env{App: "shop", Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pushed_at") {
		t.Fatalf("pushed_at leaked for an env never pushed: %s", b)
	}
}
