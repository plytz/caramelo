package config

import "testing"

func TestParseV4(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
placement: hub
envs:
  production:
    machine: nx2
    hosts: [shop.example.com]
    via: hub
  staging: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if app.PlacementOf() != PlacementHub {
		t.Errorf("placement = %q, want %q", app.PlacementOf(), PlacementHub)
	}
	if got := app.MachineOf("production"); got != "nx2" {
		t.Errorf("machine of production = %q, want %q", got, "nx2")
	}
	if got := app.ViaOf("production"); got != ViaHub {
		t.Errorf("via of production = %q, want %q", got, ViaHub)
	}

	for _, env := range []string{"staging", "feat-x"} {
		if got := app.ViaOf(env); got != ViaNode {
			t.Errorf("via of %s = %q, want %q", env, got, ViaNode)
		}
		if got := app.MachineOf(env); got != "" {
			t.Errorf("machine of %s = %q, want the file not to pin one", env, got)
		}
	}
}

func TestV3FileReadsAsV4(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
domain: shop.example.com
envs:
  production:
    hosts: [shop.example.com]
    deploy: { watch: 5m }
`))
	if err != nil {
		t.Fatal(err)
	}
	if app.PlacementOf() != PlacementAuto {
		t.Errorf("placement = %q, want %q", app.PlacementOf(), PlacementAuto)
	}
	if app.ViaOf("production") != ViaNode || app.MachineOf("production") != "" {
		t.Errorf("v3 production = via %q machine %q, want %q and none",
			app.ViaOf("production"), app.MachineOf("production"), ViaNode)
	}

	if o, _ := app.Override("production"); o.Empty() {
		t.Error("a v3 override with hosts and a deploy policy read as empty")
	}
}

func TestV4RefusesBadValues(t *testing.T) {
	for name, doc := range map[string]string{
		"placement": "placement: anywhere\n",
		"via":       "envs:\n  production:\n    via: sideways\n",
		"machine":   "envs:\n  production:\n    machine: Not A Slug\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: a bad value was accepted", name)
		}
	}
}

func TestValidateFleetOnAHandBuiltApp(t *testing.T) {
	a := &App{Name: "shop", Placement: Placement("elsewhere")}
	if err := a.Validate(); err == nil {
		t.Error("a hand-built app with an unknown placement validated")
	}
	a = &App{Name: "shop", Envs: map[string]EnvOverride{"production": {Via: Via("sideways")}}}
	if err := a.Validate(); err == nil {
		t.Error("a hand-built app with an unknown via validated")
	}
	a = &App{Name: "shop", Envs: map[string]EnvOverride{"production": {Machine: "PI2"}}}
	if err := a.Validate(); err == nil {
		t.Error("a hand-built app with an impossible machine name validated")
	}
}

func TestPlacementAndViaDefaults(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Placement
	}{{"", PlacementAuto}, {"auto", PlacementAuto}, {"HUB", PlacementHub}} {
		got, err := ParsePlacement(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParsePlacement(%q) = %q, %v", tc.in, got, err)
		}
	}
	for _, tc := range []struct {
		in   string
		want Via
	}{{"", ViaNode}, {" node ", ViaNode}, {"Hub", ViaHub}} {
		got, err := ParseVia(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseVia(%q) = %q, %v", tc.in, got, err)
		}
	}
	if Placement("").String() != string(PlacementAuto) || Via("").String() != string(ViaNode) {
		t.Error("the zero value must print as the default")
	}
}

func TestFleetSettings(t *testing.T) {
	app, err := Parse([]byte(`
name: shop
placement: hub
envs:
  production:
    machine: nx2
    via: hub
  staging:
    machine: hub
  preview:
    hosts: [preview.shop.example]
`))
	if err != nil {
		t.Fatal(err)
	}
	got := app.FleetSettings()
	want := []FleetSetting{
		{Key: "placement", Value: "hub", FromFile: true},
		{Key: "envs.production.machine", Value: "nx2", FromFile: true},
		{Key: "envs.production.via", Value: "hub", FromFile: true,
			Evidence: "the hub's edge serves the names and forwards through the tunnel"},
		{Key: "envs.staging.machine", Value: "hub", FromFile: true,
			Evidence: "the hub, whichever machine that is"},
	}
	if len(got) != len(want) {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("setting %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFleetSettingsOfAV3File(t *testing.T) {
	app, err := Parse([]byte("name: shop\nenvs:\n  production:\n    hosts: [shop.example]\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := app.FleetSettings()
	if len(got) != 1 {
		t.Fatalf("settings = %+v, want the placement alone", got)
	}
	if got[0].Key != "placement" || got[0].Value != "auto" || got[0].FromFile || got[0].Evidence == "" {
		t.Errorf("setting = %+v", got[0])
	}
	if len((*App)(nil).FleetSettings()) != 0 {
		t.Error("a nil app has no settings")
	}
}

func TestMachineHub(t *testing.T) {
	app, err := Parse([]byte("name: shop\nenvs:\n  production:\n    machine: hub\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app.MachineOf("production"); got != MachineHub {
		t.Errorf("machine = %q, want %q", got, MachineHub)
	}
}
