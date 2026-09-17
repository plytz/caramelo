package vpnclient

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const probeMachineKey = "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="

func probeClient(t *testing.T) (*client, *FileRecordStore) {
	t.Helper()
	keys, records := stores(t)
	c, err := NewWith(Options{Control: &fakeControl{}, Keys: keys, Records: records})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*client), records
}

func admit(t *testing.T, records *FileRecordStore, machine string) {
	t.Helper()
	rec := Record{
		Machine:    machine,
		Endpoint:   "203.0.113.9:4021",
		MachineKey: probeMachineKey,
		Subnet:     netip.MustParsePrefix("10.86.0.0/16"),
		MachineIP:  netip.MustParseAddr("10.86.0.1"),
		IP:         netip.MustParseAddr("10.86.0.2"),
		PublicKey:  probeMachineKey,
		APIPort:    4022,
	}
	if err := records.Save(rec); err != nil {
		t.Fatal(err)
	}
}

func stubHandshake(t *testing.T, at time.Time, err error) {
	t.Helper()
	prev := probeHandshake
	probeHandshake = func(context.Context, *client, Record, time.Duration) (time.Time, error) {
		return at, err
	}
	t.Cleanup(func() { probeHandshake = prev })
}

func stubLookup(t *testing.T, err error) {
	t.Helper()
	prev := probeLookup
	probeLookup = func(context.Context, string) ([]netip.Addr, error) {
		if err != nil {
			return nil, err
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil
	}
	t.Cleanup(func() { probeLookup = prev })
}

func TestProbeAnswersReachedOnlyWhenAPacketCameBack(t *testing.T) {
	c, records := probeClient(t)
	admit(t, records, "box")
	when := time.Now().Add(-time.Second)
	stubHandshake(t, when, nil)

	p, err := c.Probe(context.Background(), ProbeRequest{Machine: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Result != ProbeReached {
		t.Fatalf("result = %q (%s), want %q", p.Result, p.Detail, ProbeReached)
	}
	if !p.Handshake.Equal(when) || !p.Admitted {
		t.Errorf("probe = %+v, want the handshake it saw and an admitted commander", p)
	}
	if !strings.Contains(p.Detail, "udp 4021") {
		t.Errorf("detail = %q, want the port it arrived on", p.Detail)
	}
}

func TestProbeSaysNoAnswerWhenTheCommanderIsAdmitted(t *testing.T) {
	c, records := probeClient(t)
	admit(t, records, "box")
	stubHandshake(t, time.Time{}, errors.New("nothing came back"))

	p, err := c.Probe(context.Background(), ProbeRequest{Machine: "box", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if p.Result != ProbeNoAnswer {
		t.Fatalf("result = %q (%s), want %q", p.Result, p.Detail, ProbeNoAnswer)
	}
	for _, want := range []string{"udp 4021", "firewall", "security group"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", p.Detail, want)
		}
	}
}

func TestProbeSaysUnprovenWhenThisCommanderIsNotAdmitted(t *testing.T) {
	c, _ := probeClient(t)
	stubLookup(t, nil)
	stubHandshake(t, time.Time{}, errors.New("nothing came back"))

	p, err := c.Probe(context.Background(), ProbeRequest{Machine: "box.example:4021", Key: probeMachineKey})
	if err != nil {
		t.Fatal(err)
	}
	if p.Result != ProbeUnproven {
		t.Fatalf("result = %q (%s), want %q", p.Result, p.Detail, ProbeUnproven)
	}
	if p.Admitted {
		t.Error("a commander with no record claims it is admitted")
	}
	for _, want := range []string{"silence proves nothing", "caramelo peer add"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", p.Detail, want)
		}
	}
}

func TestProbeSaysNoRouteWhenTheNameDoesNotResolve(t *testing.T) {
	c, _ := probeClient(t)
	stubLookup(t, errors.New("no such host"))
	stubHandshake(t, time.Now(), nil)

	p, err := c.Probe(context.Background(), ProbeRequest{Machine: "nowhere.invalid", Key: probeMachineKey})
	if err != nil {
		t.Fatal(err)
	}
	if p.Result != ProbeNoRoute {
		t.Fatalf("result = %q (%s), want %q", p.Result, p.Detail, ProbeNoRoute)
	}
	if !strings.Contains(p.Detail, "does not resolve") {
		t.Errorf("detail = %q, want it to say the name does not resolve", p.Detail)
	}
	if p.Endpoint != "nowhere.invalid:4021" {
		t.Errorf("endpoint = %q, want the tunnel's default port added", p.Endpoint)
	}
}

func TestProbeNeedsAKeyItDoesNotHave(t *testing.T) {
	c, _ := probeClient(t)
	_, err := c.Probe(context.Background(), ProbeRequest{Machine: "box.example:4021"})
	if err == nil || !strings.Contains(err.Error(), "--key") {
		t.Fatalf("err = %v, want it to ask for the machine's public key", err)
	}

	_, err = c.Probe(context.Background(), ProbeRequest{Machine: "box.example:4021", Key: "not-a-key"})
	if err == nil || !strings.Contains(err.Error(), "not a WireGuard public key") {
		t.Fatalf("err = %v, want it to refuse a key that is not one", err)
	}
}

func TestProbeOverridesUseAnUnprovenRecord(t *testing.T) {
	c, records := probeClient(t)
	admit(t, records, "box")
	stubLookup(t, nil)
	stubHandshake(t, time.Time{}, errors.New("nothing came back"))

	p, err := c.Probe(context.Background(), ProbeRequest{Machine: "box", Endpoint: "198.51.100.7:4021"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Result != ProbeUnproven {
		t.Errorf("result = %q, want %q: an endpoint the record does not name is not an admitted one", p.Result, ProbeUnproven)
	}
	if p.Endpoint != "198.51.100.7:4021" {
		t.Errorf("endpoint = %q, want the one that was asked for", p.Endpoint)
	}
}

func TestProbeNeedsSomethingToProbe(t *testing.T) {
	c, _ := probeClient(t)
	if _, err := c.Probe(context.Background(), ProbeRequest{}); err == nil {
		t.Fatal("a probe with no machine must fail")
	}
}

func TestNoHandshakeNamesTheFirewallAndTheProbe(t *testing.T) {
	rec := Record{Machine: "box", Endpoint: "192.168.56.11:4021"}
	msg := noHandshake(rec, 10*time.Second).Error()
	for _, want := range []string{"no handshake with box", "192.168.56.11:4021", "peer list",
		"firewall", "security group", "caramelo hub probe box"} {
		if !strings.Contains(msg, want) {
			t.Errorf("noHandshake = %q, want it to mention %q", msg, want)
		}
	}
}
