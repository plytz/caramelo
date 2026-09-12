package config

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestParseV2(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
domain: shop.example.com
services:
  web:
    run: npm start
    port: 3000
    replicas: 2
    expose: https
    drain: 45s
  api:
    run: npm run api
    port: 4000
  worker:
    run: npm run worker
    port: none
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if app.Domain != "shop.example.com" {
		t.Errorf("domain = %q", app.Domain)
	}
	web, _ := app.Service("web")
	if got := web.Replicas(); got != 2 {
		t.Errorf("web replicas = %d, want 2", got)
	}
	if got := web.DrainTimeout(); got != 45*time.Second {
		t.Errorf("web drain = %s, want 45s", got)
	}

	api, _ := app.Service("api")
	if got := api.Replicas(); got != DefaultReplicas {
		t.Errorf("api replicas = %d, want %d", got, DefaultReplicas)
	}
	if got := api.DrainTimeout(); got != DefaultDrain {
		t.Errorf("api drain = %s, want %s", got, DefaultDrain)
	}
	for name, want := range map[string]Expose{"web": ExposeHTTPS, "api": ExposeNone, "worker": ExposeNone} {
		if got := app.ExposeOf(name); got != want {
			t.Errorf("ExposeOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExposeDefaults(t *testing.T) {
	file := `
name: shop
%s
services:
  worker:
    run: npm run worker
    port: none
  web:
    run: npm start
    port: 3000
  api:
    run: npm run api
    port: 4000
`
	withDomain, err := Parse([]byte(strings.Replace(file, "%s", "domain: shop.example.com", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := withDomain.ExposeOf("web"); got != ExposeHTTPS {
		t.Errorf("the first service with a port = %q, want %q", got, ExposeHTTPS)
	}
	for _, name := range []string{"worker", "api"} {
		if got := withDomain.ExposeOf(name); got != ExposeNone {
			t.Errorf("ExposeOf(%q) = %q, want %q: only one name is given away by default", name, got, ExposeNone)
		}
	}
	if got := withDomain.ExposedServices(); len(got) != 1 || got[0].Name != "web" {
		t.Errorf("ExposedServices() = %+v, want just web", got)
	}

	noDomain, err := Parse([]byte(strings.Replace(file, "%s", "", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := noDomain.ExposeOf("web"); got != ExposeNone {
		t.Errorf("ExposeOf(web) = %q on an app with no domain, want %q", got, ExposeNone)
	}
	if got := noDomain.ExposedServices(); len(got) != 0 {
		t.Errorf("ExposedServices() = %+v on an app with no domain, want none", got)
	}
}

func TestExposeNoneDoesNotConsumeTheDefault(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
domain: shop.example.com
services:
  worker:
    run: npm run worker
    port: 9000
    expose: none
  web:
    run: npm start
    port: 3000
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := app.ExposeOf("web"); got != ExposeHTTPS {
		t.Errorf("ExposeOf(web) = %q, want %q: the worker is off the edge, so web is the first exposable one", got, ExposeHTTPS)
	}
	if got := app.ExposeOf("worker"); got != ExposeNone {
		t.Errorf("ExposeOf(worker) = %q, want %q", got, ExposeNone)
	}
	if got := app.ExposedServices(); len(got) != 1 || got[0].Name != "web" {
		t.Errorf("ExposedServices() = %+v, want just web", got)
	}
	if got := app.HostFor("feat-x", "web"); got != "feat-x.shop.example.com" {
		t.Errorf("HostFor(feat-x, web) = %q, want the env's short name", got)
	}
}

func TestHostFor(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
domain: shop.example.com
services:
  web:
    run: npm start
    port: 3000
  api:
    run: npm run api
    port: 4000
    expose: https
  worker:
    run: npm run worker
    port: none
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, want := range map[string]string{
		"web":     "feat-x.shop.example.com",
		"api":     "api.feat-x.shop.example.com",
		"worker":  "",
		"unknown": "",
	} {
		if got := app.HostFor("feat-x", name); got != want {
			t.Errorf("HostFor(feat-x, %q) = %q, want %q", name, got, want)
		}
	}

	if got := DerivedHost("Shop.Example.com.", "feat-y", "web", true); got != "feat-y.shop.example.com" {
		t.Errorf("DerivedHost = %q", got)
	}
	if got := DerivedHost("", "feat-y", "web", true); got != "" {
		t.Errorf("DerivedHost with no domain = %q, want the empty string", got)
	}
}

func TestAutoDomain(t *testing.T) {
	got, err := AutoDomain("shop", netip.MustParseAddr("192.0.2.10"))
	if err != nil {
		t.Fatalf("AutoDomain: %v", err)
	}
	if want := "shop.192-0-2-10.sslip.io"; got != want {
		t.Errorf("AutoDomain = %q, want %q", got, want)
	}

	if host := DerivedHost(got, "feat-x", "web", true); host != "feat-x.shop.192-0-2-10.sslip.io" {
		t.Errorf("derived name = %q", host)
	}
	if _, err := AutoDomain("shop", netip.Addr{}); err == nil {
		t.Error("AutoDomain with no address = nil, want an error")
	}
	if _, err := AutoDomain("", netip.MustParseAddr("192.0.2.10")); err == nil {
		t.Error("AutoDomain with no app name = nil, want an error")
	}
}

func TestReservedExposeKinds(t *testing.T) {
	for _, value := range []string{"tls", "tls-passthrough", "tcp", "tcp:5432", "udp", "udp:9000"} {
		_, err := ParseExpose(value)
		if err == nil {
			t.Errorf("ParseExpose(%q) = nil, want %q", value, "not supported yet")
			continue
		}
		if !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("ParseExpose(%q) = %v, want it to say it is not supported yet", value, err)
		}
	}
	if _, err := ParseExpose("gopher"); err == nil || strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("ParseExpose(gopher) = %v, want an unknown-value error", err)
	}

	_, err := Parse([]byte("name: shop\ndomain: shop.example.com\nservices:\n  db:\n    run: x\n    port: 5432\n    expose: tcp:5432\n"))
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("Parse with expose: tcp = %v, want a refusal", err)
	}
}

func TestValidateHostname(t *testing.T) {
	for _, host := range []string{"shop.example.com", "feat-x.shop.example.com", "shop.192-0-2-10.sslip.io", "a.b"} {
		if err := ValidateHostname(host); err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want nil", host, err)
		}
	}
	for name, host := range map[string]string{
		"empty":          "",
		"a URL":          "https://shop.example.com",
		"a port":         "shop.example.com:8443",
		"one label":      "shop",
		"a trailing dot": "shop.example.com.",
		"an empty label": "shop..example.com",
		"uppercase":      "Shop.example.com",
		"a leading dash": "-shop.example.com",
		"a space":        "shop example.com",
		"a long label":   strings.Repeat("a", MaxLabelLen+1) + ".example.com",
		"a long name":    strings.Repeat("a.", 130) + "example.com",
	} {
		if err := ValidateHostname(host); err == nil {
			t.Errorf("%s: ValidateHostname(%q) = nil, want an error", name, host)
		}
	}
}

func TestV2Validation(t *testing.T) {
	for name, file := range map[string]string{
		"replicas out of range":          "name: shop\nservices:\n  web:\n    run: x\n    port: 3000\n    replicas: 99\n",
		"replicas below one":             "name: shop\nservices:\n  web:\n    run: x\n    port: 3000\n    replicas: 0\n",
		"a drain that is not a duration": "name: shop\nservices:\n  web:\n    run: x\n    port: 3000\n    drain: 30\n",
		"a negative drain":               "name: shop\nservices:\n  web:\n    run: x\n    port: 3000\n    drain: -5s\n",
		"a domain that is a URL":         "name: shop\ndomain: https://shop.example.com\n",
		"a domain with a port":           "name: shop\ndomain: shop.example.com:8443\n",
		"a domain with one label":        "name: shop\ndomain: shop\n",
		"an exposed service with no port": "name: shop\ndomain: shop.example.com\nservices:\n" +
			"  web:\n    run: x\n    port: none\n    expose: https\n",
		"an exposed UDP service": "name: shop\ndomain: shop.example.com\nservices:\n" +
			"  web:\n    run: x\n    port: 3000\n    protocol: udp\n    expose: https\n",
		"an exposed service with no domain": "name: shop\nservices:\n  web:\n    run: x\n    port: 3000\n    expose: https\n",
	} {
		if _, err := Parse([]byte(file)); err == nil {
			t.Errorf("%s: Parse = nil, want an error", name)
		}
	}

	for name, file := range map[string]string{
		"auto":         "name: shop\ndomain: auto\nservices:\n  web:\n    run: x\n    port: 3000\n",
		"expose: none": "name: shop\ndomain: shop.example.com\nservices:\n  web:\n    run: x\n    port: none\n    expose: none\n",
	} {
		if _, err := Parse([]byte(file)); err != nil {
			t.Errorf("%s: Parse = %v, want nil", name, err)
		}
	}
}
