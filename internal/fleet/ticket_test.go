package fleet

import (
	"strings"
	"testing"
)

func aTicket() Ticket {
	return Ticket{
		Hub:       "nx1",
		Fleet:     "home",
		Endpoint:  "hub.example.com:4021",
		PublicKey: "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9=",
		Address:   "10.86.0.1",
		Peer:      "10.86.0.9",
		Range:     FleetRange,
		Secret:    "s3cr3t",
	}
}

func TestTicketRoundTrips(t *testing.T) {
	want := aTicket()
	s, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(s, TicketPrefix) {
		t.Fatalf("ticket %q does not start with %q", s, TicketPrefix)
	}
	if strings.ContainsAny(s, " \t\n") {
		t.Fatalf("ticket %q is not one word a person can copy", s)
	}
	got, err := ParseTicket("  " + s + "\n")
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if got != want {
		t.Fatalf("ticket = %+v, want %+v", got, want)
	}
	r, err := got.RangePrefix()
	if err != nil || r.String() != FleetRange {
		t.Errorf("range = %v (%v), want %s", r, err, FleetRange)
	}
}

func TestTicketWithNoRange(t *testing.T) {
	tk := aTicket()
	tk.Range = ""
	r, err := tk.RangePrefix()
	if err != nil || r.String() != FleetRange {
		t.Fatalf("range = %v (%v), want the default %s", r, err, FleetRange)
	}
}

func TestParseTicketRefusesWhatIsNotOne(t *testing.T) {
	cases := map[string]string{
		"something else":  "not-a-token",
		"a damaged blob":  TicketPrefix + "!!!!",
		"an empty prefix": TicketPrefix,
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTicket(s); err == nil {
				t.Fatalf("ParseTicket accepted %q", s)
			}
		})
	}
}

func TestTicketValidate(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Ticket)
		want   string
	}{
		"no hub":        {func(t *Ticket) { t.Hub = "" }, "name the hub"},
		"no fleet":      {func(t *Ticket) { t.Fleet = "" }, "name the fleet"},
		"no secret":     {func(t *Ticket) { t.Secret = "" }, "no secret"},
		"no key":        {func(t *Ticket) { t.PublicKey = "" }, "public key"},
		"no endpoint":   {func(t *Ticket) { t.Endpoint = "" }, "where to dial"},
		"a bare host":   {func(t *Ticket) { t.Endpoint = "hub.example.com" }, "want host:port"},
		"a silly port":  {func(t *Ticket) { t.Endpoint = "h:0" }, "out of range"},
		"no address":    {func(t *Ticket) { t.Address = "" }, "hub's address"},
		"no peer":       {func(t *Ticket) { t.Peer = "" }, "hub's peer"},
		"a bad address": {func(t *Ticket) { t.Address = "over there" }, "over there"},
		"a bad range":   {func(t *Ticket) { t.Range = "10.80.0.0" }, "range"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tk := aTicket()
			tc.mutate(&tk)
			err := tk.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tk)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, want it to mention %q", err, tc.want)
			}

			if _, err := tk.Encode(); err == nil {
				t.Fatal("Encode produced a ticket Validate refuses")
			}
		})
	}
}

func TestTicketRedacted(t *testing.T) {
	r := aTicket().Redacted()
	if r.Secret == "s3cr3t" {
		t.Error("the secret survived redaction")
	}
	if r.Hub != "nx1" || r.Endpoint != "hub.example.com:4021" || r.Address != "10.86.0.1" {
		t.Errorf("redaction took more than the secret: %+v", r)
	}
}
