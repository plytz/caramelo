package config

import "testing"

func TestKnownImagePasswordVar(t *testing.T) {
	for _, image := range []string{"postgres:16-alpine", "mysql:8", "mariadb:11"} {
		k, ok := Lookup(image)
		if !ok {
			t.Fatalf("%s is not a known image", image)
		}
		if k.PasswordVar == "" {
			t.Errorf("%s has a default password and no PasswordVar", image)
		}
		value, has := k.Password()
		if !has || value != "caramelo" {
			t.Errorf("%s: Password() = %q, %v", image, value, has)
		}

		if _, ok := k.Env[k.PasswordVar]; !ok {
			t.Errorf("%s names %q, which is not in its Env", image, k.PasswordVar)
		}
	}

	for _, image := range []string{"redis:7-alpine", "mongo:7", "rabbitmq:3"} {
		k, ok := Lookup(image)
		if !ok {
			t.Fatalf("%s is not a known image", image)
		}
		if k.PasswordVar != "" {
			t.Errorf("%s names the password variable %q", image, k.PasswordVar)
		}
		if _, has := k.Password(); has {
			t.Errorf("%s reports a password", image)
		}
	}

	if _, ok := Lookup("nats:2"); ok {
		t.Error("nats is in the known table now; this test needs a different example")
	}
}

func TestDepPasswordSecretIsAValidName(t *testing.T) {
	for dep, want := range map[string]string{
		"db": "DB_PASSWORD", "cache": "CACHE_PASSWORD", "my-db": "MY_DB_PASSWORD",
	} {
		if got := DepPasswordSecret(dep); got != want {
			t.Errorf("DepPasswordSecret(%q) = %q, want %q", dep, got, want)
		}
	}
}
