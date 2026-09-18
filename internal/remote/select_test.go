package remote

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

type fakeDialer struct{ machine string }

func (fakeDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("fakeDialer does not dial")
}

func (d fakeDialer) MachineAddr() string { return d.machine }

func selection() Selection {
	return Selection{
		SocketPath:   "/run/caramelo/caramelod.sock",
		SocketUser:   "caramelo",
		SocketExists: func(string) bool { return false },
		Tunnel:       func(context.Context, string, Target) (Dialer, error) { return nil, ErrNoTunnel },
	}
}

func twoFleets() CommanderConfig {
	return CommanderConfig{
		Name: "laptop",
		Role: RoleCommander,
		Commander: Commander{
			Fleets: map[string]Fleet{
				"home": {Hub: "caramelo@box:4022", Apps: []string{"shop"}},
				"work": {Hub: "ops@hub.work.example:4023", Apps: []string{"blog"}},
			},
		},
	}
}

func TestSelectOrder(t *testing.T) {
	ctx := context.Background()
	home := Target{User: "caramelo", Host: "box", Port: 4022}
	work := Target{User: "ops", Host: "hub.work.example", Port: 4023}

	t.Run("machine local is the socket", func(t *testing.T) {
		s := selection()
		s.Machine = "local"
		s.SocketExists = func(string) bool { return true }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSocket || got.SocketPath != s.SocketPath || got.User != s.SocketUser {
			t.Fatalf("got %+v, want the local socket", got)
		}
	})

	t.Run("a raw machine beats the local socket and is in no fleet", func(t *testing.T) {
		s := selection()
		s.Machine = "alex@box"
		s.Config = twoFleets()
		s.SocketExists = func(string) bool { return true }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		want := Target{User: "alex", Host: "box", Port: 4022}
		if got.Kind != KindSSH || got.Target != want {
			t.Fatalf("got %+v, want ssh to %v", got, want)
		}
		if got.Fleet != "" {
			t.Errorf("fleet = %q, want none: --machine names a box that is in no fleet yet", got.Fleet)
		}
	})

	t.Run("--fleet beats the local socket", func(t *testing.T) {
		s := selection()
		s.Fleet = "work"
		s.Config = twoFleets()
		s.SocketExists = func(string) bool { return true }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSSH || got.Target != work || got.Fleet != "work" {
			t.Fatalf("got %+v, want ssh to the hub of work", got)
		}
		if !strings.Contains(got.Why, "--fleet") {
			t.Errorf("why = %q, want it to name the rule", got.Why)
		}
	})

	t.Run("--machine and --fleet together are refused", func(t *testing.T) {
		s := selection()
		s.Machine, s.Fleet, s.Config = "alex@box", "home", twoFleets()
		_, err := Select(ctx, s)
		if err == nil {
			t.Fatal("Select accepted both --machine and --fleet")
		}
		for _, want := range []string{"--machine", "--fleet"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to name %s", err, want)
			}
		}
	})

	t.Run("a fleet that is not configured is refused, naming the ones that are", func(t *testing.T) {
		s := selection()
		s.Fleet = "nowhere"
		s.Config = twoFleets()
		_, err := Select(ctx, s)
		if err == nil {
			t.Fatal("Select accepted a fleet that is in no config")
		}
		if !strings.Contains(err.Error(), "home, work") {
			t.Errorf("error = %v, want it to name the configured fleets", err)
		}
	})

	t.Run("a local socket beats the app and the default", func(t *testing.T) {
		s := selection()
		s.SocketExists = func(string) bool { return true }
		s.Config = twoFleets()
		s.Config.Commander.DefaultFleet = "home"
		s.App = func() string { return "blog" }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSocket {
			t.Fatalf("got %+v, want the local socket", got)
		}
	})

	t.Run("the fleet recorded for the app beats the default", func(t *testing.T) {
		s := selection()
		s.Config = twoFleets()
		s.Config.Commander.DefaultFleet = "home"
		s.App = func() string { return "blog" }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Target != work || got.Fleet != "work" {
			t.Fatalf("got %+v, want the fleet recorded for blog", got)
		}
		if !strings.Contains(got.Why, "blog") {
			t.Errorf("why = %q, want it to name the app", got.Why)
		}
	})

	t.Run("an app recorded on two fleets does not decide", func(t *testing.T) {
		s := selection()
		s.Config = twoFleets()
		s.Config.Commander.Fleets["work"] = Fleet{Hub: "ops@hub.work.example:4023", Apps: []string{"blog", "shop"}}
		s.Config.Commander.DefaultFleet = "home"
		s.App = func() string { return "shop" }
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Fleet != "home" {
			t.Fatalf("fleet = %q, want the default: an app seen on two fleets picks neither", got.Fleet)
		}
	})

	t.Run("default_fleet when nothing else says", func(t *testing.T) {
		s := selection()
		s.Config = twoFleets()
		s.Config.Commander.DefaultFleet = "home"
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != KindSSH || got.Target != home || got.Fleet != "home" {
			t.Fatalf("got %+v, want ssh to %v", got, home)
		}
		if !strings.Contains(got.Why, "commander.default_fleet") {
			t.Errorf("why = %q, want it to name the rule", got.Why)
		}
	})

	t.Run("the only fleet needs no default", func(t *testing.T) {
		s := selection()
		s.Config = CommanderConfig{Commander: Commander{
			Fleets: map[string]Fleet{"home": {Hub: "caramelo@box:4022"}},
		}}
		got, err := Select(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Target != home || got.Fleet != "home" {
			t.Fatalf("got %+v, want ssh to the only fleet", got)
		}
		if !strings.Contains(got.Why, "only fleet") {
			t.Errorf("why = %q, want it to name the rule", got.Why)
		}
	})

	t.Run("several fleets and nothing to pick one is a refusal naming them", func(t *testing.T) {
		s := selection()
		s.Config = twoFleets()
		_, err := Select(ctx, s)
		if err == nil {
			t.Fatal("Select guessed between two fleets")
		}
		for _, want := range []string{"home, work", "--fleet", "fleet default"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to say %q", err, want)
			}
		}
	})

	t.Run("nothing configured", func(t *testing.T) {
		if _, err := Select(ctx, selection()); !errors.Is(err, ErrNoFleet) {
			t.Fatalf("err = %v, want ErrNoFleet", err)
		}
	})

	t.Run("an unparseable machine is an error", func(t *testing.T) {
		s := selection()
		s.Machine = "alex@box:notaport"
		if _, err := Select(ctx, s); err == nil {
			t.Fatal("Select accepted a machine with a bad port")
		}
	})
}

func TestSelectPrefersTheTunnel(t *testing.T) {
	ctx := context.Background()
	dialer := fakeDialer{}

	for _, tc := range []struct {
		name string
		mut  func(*Selection)
	}{
		{"--machine", func(s *Selection) { s.Machine = "box" }},
		{"default_fleet", func(s *Selection) {
			s.Config = CommanderConfig{Commander: Commander{
				DefaultFleet: "home",
				Fleets:       map[string]Fleet{"home": {Hub: "caramelo@box:4022"}},
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := selection()
			var asked Target
			s.Tunnel = func(_ context.Context, _ string, target Target) (Dialer, error) {
				asked = target
				return dialer, nil
			}
			tc.mut(&s)
			got, err := Select(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != KindTunnel {
				t.Fatalf("kind = %q, want %q", got.Kind, KindTunnel)
			}
			if got.Dialer != Dialer(dialer) {
				t.Error("the choice did not carry the tunnel's dialer")
			}
			if asked.Host != "box" {
				t.Errorf("the tunnel was asked for %v, want the machine", asked)
			}
			if !strings.Contains(got.Why, "peer key") {
				t.Errorf("why = %q, want it to say a peer key picked the tunnel", got.Why)
			}
		})
	}

	t.Run("the socket never asks for a tunnel", func(t *testing.T) {
		s := selection()
		s.SocketExists = func(string) bool { return true }
		s.Tunnel = func(context.Context, string, Target) (Dialer, error) {
			t.Error("the local socket asked for a tunnel")
			return nil, ErrNoTunnel
		}
		got, err := Select(ctx, s)
		if err != nil || got.Kind != KindSocket {
			t.Fatalf("got %+v, %v; want the local socket", got, err)
		}
	})

	t.Run("no tunnel falls through to ssh", func(t *testing.T) {
		s := selection()
		s.Machine = "box"

		s.Tunnel = func(context.Context, string, Target) (Dialer, error) { return nil, nil }
		got, err := Select(ctx, s)
		if err != nil || got.Kind != KindSSH {
			t.Fatalf("got %+v, %v; want ssh", got, err)
		}
	})

	t.Run("a broken tunnel is an error, not a fall-through", func(t *testing.T) {
		boom := errors.New("no handshake with box after 10s")
		s := selection()
		s.Machine = "box"
		s.Tunnel = func(context.Context, string, Target) (Dialer, error) { return nil, boom }
		_, err := Select(ctx, s)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the tunnel's own error", err)
		}
		if !strings.Contains(err.Error(), "box") {
			t.Errorf("err = %v, want it to name the machine", err)
		}
	})
}

func TestTunnelTransportAddress(t *testing.T) {
	target := Target{User: "caramelo", Host: "box.example.com", Port: 4022}

	tr := TunnelTransport(fakeDialer{}, target)
	if tr.Kind() != KindTunnel {
		t.Errorf("Kind() = %q, want %q", tr.Kind(), KindTunnel)
	}
	if tr.Address != "10.86.0.1:4022" {
		t.Errorf("Address = %q, want the machine's address inside the default subnet", tr.Address)
	}
	if tr.User != target.User {
		t.Errorf("User = %q, want %q", tr.User, target.User)
	}
	if !strings.Contains(tr.String(), "box.example.com") {
		t.Errorf("String() = %q, want it to name the machine", tr.String())
	}

	tr = TunnelTransport(fakeDialer{machine: "10.99.0.1"}, Target{User: "caramelo", Host: "box", Port: 2222})
	if tr.Address != "10.99.0.1:2222" {
		t.Errorf("Address = %q, want the dialer's machine address and the target's port", tr.Address)
	}
	tr = TunnelTransport(fakeDialer{machine: "box.internal"}, target)
	if tr.Address != "box.internal:4022" {
		t.Errorf("Address = %q, want the name the dialer resolves", tr.Address)
	}
}

func TestPlanAnswersTheSameAsSelectAndNeverDials(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		mut  func(*Selection)
	}{
		{"the local socket", func(s *Selection) { s.SocketExists = func(string) bool { return true } }},
		{"--machine", func(s *Selection) { s.Machine = "alex@10.0.0.5:4023" }},
		{"--fleet", func(s *Selection) { s.Fleet = "work" }},
		{"the app's fleet", func(s *Selection) { s.App = func() string { return "blog" } }},
		{"the default fleet", func(s *Selection) { s.Config.Commander.DefaultFleet = "home" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := selection()
			s.Config = twoFleets()
			tc.mut(&s)
			s.Tunnel = func(context.Context, string, Target) (Dialer, error) { return nil, ErrNoTunnel }
			want, err := Select(ctx, s)
			if err != nil {
				t.Fatal(err)
			}

			s.Tunnel = func(context.Context, string, Target) (Dialer, error) {
				t.Error("Plan dialed a tunnel")
				return nil, ErrNoTunnel
			}
			got, err := s.Plan()
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != want.Kind || got.Target != want.Target || got.Fleet != want.Fleet || got.Why != want.Why {
				t.Errorf("Plan() = %+v, want the choice Select made: %+v", got, want)
			}
		})
	}
}

func TestPlanRefusesToGuessBetweenFleets(t *testing.T) {
	s := selection()
	s.Config = twoFleets()
	_, err := s.Plan()
	if err == nil {
		t.Fatal("want a refusal when two fleets are configured and nothing picks one")
	}
	for _, want := range []string{"home, work", "--fleet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	s.Config = CommanderConfig{}
	if _, err := s.Plan(); !errors.Is(err, ErrNoFleet) {
		t.Errorf("err = %v, want ErrNoFleet with nothing configured", err)
	}
}
