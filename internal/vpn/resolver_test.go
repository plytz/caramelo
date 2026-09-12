package vpn

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func testZone(t *testing.T) *ZoneResolver {
	t.Helper()
	r := NewResolver()
	err := r.SetZone(context.Background(), []Address{
		{
			IP:    netip.MustParseAddr("10.86.0.1"),
			Kind:  KindMachine,
			Owner: "worker1",
			Names: []string{MachineHost("worker1")},
		},
		{
			IP:    netip.MustParseAddr("10.86.1.4"),
			Kind:  KindEnv,
			Owner: "shop/feat-x",
			Names: EnvHosts("shop", "feat-x", []string{"web", "db"}),
		},
	})
	if err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	return r
}

func TestResolveTheZone(t *testing.T) {
	r := testZone(t)
	ctx := context.Background()
	for name, want := range map[string]string{
		"worker1.internal":          "10.86.0.1",
		"feat-x.shop.internal":      "10.86.1.4",
		"web.feat-x.shop.internal":  "10.86.1.4",
		"db.feat-x.shop.internal":   "10.86.1.4",
		"FEAT-X.SHOP.INTERNAL":      "10.86.1.4",
		"feat-x.shop.internal.":     "10.86.1.4",
		"  db.feat-x.shop.internal": "10.86.1.4",
	} {
		got, err := r.Resolve(ctx, name)
		if err != nil {
			t.Errorf("Resolve(%q): %v", name, err)
			continue
		}
		if len(got) != 1 || got[0].String() != want {
			t.Errorf("Resolve(%q) = %v, want [%s]", name, got, want)
		}
	}
}

func TestResolverIsAuthoritativeAndNeverForwards(t *testing.T) {
	r := testZone(t)
	ctx := context.Background()
	for _, name := range []string{
		"nope.internal",
		"gone.feat-x.shop.internal",
		"feat-y.shop.internal",

		"google.com",
		"localhost",
		"shop.internal.example.com",
		"internal",
		"",
	} {
		if _, err := r.Resolve(ctx, name); !errors.Is(err, ErrNXDOMAIN) {
			t.Errorf("Resolve(%q) error = %v, want ErrNXDOMAIN", name, err)
		}
	}
}

func TestSetZoneReplacesEverything(t *testing.T) {
	r := testZone(t)
	ctx := context.Background()

	err := r.SetZone(ctx, []Address{{
		IP:    netip.MustParseAddr("10.86.0.1"),
		Kind:  KindMachine,
		Names: []string{MachineHost("worker1")},
	}})
	if err != nil {
		t.Fatalf("SetZone: %v", err)
	}
	if _, err := r.Resolve(ctx, "feat-x.shop.internal"); !errors.Is(err, ErrNXDOMAIN) {
		t.Errorf("a destroyed env still resolves: %v", err)
	}
	if got := r.Names(); len(got) != 1 || got[0] != "worker1.internal" {
		t.Errorf("Names = %v, want [worker1.internal]", got)
	}
}

func TestSetZoneRejectsNamesItCannotAnswerFor(t *testing.T) {
	r := NewResolver()
	err := r.SetZone(context.Background(), []Address{{
		IP:    netip.MustParseAddr("10.86.1.4"),
		Names: []string{"feat-x.shop.example.com"},
	}})
	if err == nil {
		t.Fatal("a name outside .internal was accepted into the zone")
	}
}

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	err := b.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name),
		Type:  typ,
		Class: dnsmessage.ClassINET,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func parseAnswer(t *testing.T, msg []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatalf("parse questions: %v", err)
	}
	answers, err := p.AllAnswers()
	if err != nil && !errors.Is(err, dnsmessage.ErrSectionDone) {
		t.Fatalf("parse answers: %v", err)
	}
	return hdr, answers
}

func TestAnswerAQuery(t *testing.T) {
	r := testZone(t)
	msg, err := r.Answer(query(t, "db.feat-x.shop.internal.", dnsmessage.TypeA))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	hdr, answers := parseAnswer(t, msg)
	if hdr.ID != 0x1234 {
		t.Errorf("id = %#x, want 0x1234", hdr.ID)
	}
	if !hdr.Response || !hdr.Authoritative {
		t.Errorf("header = %+v, want an authoritative response", hdr)
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("rcode = %v, want success", hdr.RCode)
	}
	if len(answers) != 1 {
		t.Fatalf("answers = %v, want one A record", answers)
	}
	a, ok := answers[0].Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("answer body = %T, want an A record", answers[0].Body)
	}
	if got := netip.AddrFrom4(a.A).String(); got != "10.86.1.4" {
		t.Errorf("A = %s, want 10.86.1.4", got)
	}
	if answers[0].Header.TTL != TTL {
		t.Errorf("ttl = %d, want %d", answers[0].Header.TTL, TTL)
	}
}

func TestAnswerNXDOMAIN(t *testing.T) {
	r := testZone(t)
	for _, name := range []string{"nope.internal.", "google.com."} {
		msg, err := r.Answer(query(t, name, dnsmessage.TypeA))
		if err != nil {
			t.Fatalf("Answer(%s): %v", name, err)
		}
		hdr, answers := parseAnswer(t, msg)
		if hdr.RCode != dnsmessage.RCodeNameError {
			t.Errorf("%s: rcode = %v, want NXDOMAIN", name, hdr.RCode)
		}
		if len(answers) != 0 {
			t.Errorf("%s: answers = %v, want none", name, answers)
		}
	}
}

func TestAnswerAAAAOfAKnownNameIsEmptyNotNXDOMAIN(t *testing.T) {

	r := testZone(t)
	msg, err := r.Answer(query(t, "db.feat-x.shop.internal.", dnsmessage.TypeAAAA))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	hdr, answers := parseAnswer(t, msg)
	if hdr.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("rcode = %v, want success with no answers", hdr.RCode)
	}
	if len(answers) != 0 {
		t.Errorf("answers = %v, want none", answers)
	}

	msg, err = r.Answer(query(t, "nope.internal.", dnsmessage.TypeAAAA))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if hdr, _ := parseAnswer(t, msg); hdr.RCode != dnsmessage.RCodeNameError {
		t.Errorf("unknown name, AAAA: rcode = %v, want NXDOMAIN", hdr.RCode)
	}
}

func TestAnswerRejectsRubbish(t *testing.T) {
	r := testZone(t)
	if _, err := r.Answer([]byte{1, 2, 3}); err == nil {
		t.Fatal("a malformed query was answered")
	}
}
