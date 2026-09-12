package vault

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fleet struct {
	t      *testing.T
	hubDB  *DB
	rows   *fakeRows
	hub    *Hub
	member *Remote

	fetched []Fetch

	memberRows *fakeRows
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	db, rows := testStore(t)
	f := &fleet{t: t, hubDB: db, rows: rows, memberRows: &fakeRows{}}
	f.hub = &Hub{
		Store: db, Machine: "hub1",
		Now:   func() time.Time { return time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC) },
		Audit: func(_ context.Context, fetch Fetch) { f.fetched = append(f.fetched, fetch) },
	}
	f.member = &Remote{Hub: "hub1", Fetch: f.hub, Machine: "m1"}
	return f
}

func (f *fleet) set(ref Ref, value string) {
	f.t.Helper()
	if _, err := f.hubDB.Set(context.Background(), ref, value); err != nil {
		f.t.Fatalf("set %s %s: %v", ref.Where(), ref.Name, err)
	}
}

func TestAMembersContainerGetsItsSecretsFromTheHub(t *testing.T) {
	f := newFleet(t)
	f.set(MachineRef("REGISTRY_TOKEN"), "machine-wide")
	f.set(AppRef("shop", "STRIPE_KEY"), "sk_live_app")
	f.set(AppRef("shop", "GREETING"), "from the app")
	f.set(EnvRef("shop", "feat-x", "GREETING"), "fleet")

	values, err := f.member.Resolve(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Resolve on a member: %v", err)
	}
	want := map[string]string{
		"REGISTRY_TOKEN": "machine-wide",
		"STRIPE_KEY":     "sk_live_app",
		"GREETING":       "fleet",
	}
	for name, value := range want {
		if values[name] != value {
			t.Errorf("%s = %q, want %q", name, values[name], value)
		}
	}
	if len(values) != len(want) {
		t.Errorf("resolved %d secrets, want %d", len(values), len(want))
	}
	sources, err := f.member.Sources(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if sources["GREETING"] != ScopeEnv || sources["REGISTRY_TOKEN"] != ScopeMachine {
		t.Errorf("sources = %v, want the hub's provenance", sources)
	}
}

func TestTheHubWritesAnAuditRowNamingTheMember(t *testing.T) {
	f := newFleet(t)
	f.set(EnvRef("shop", "feat-x", "GREETING"), "fleet")

	ctx := WithReason(context.Background(), "up")
	if _, err := f.member.Resolve(ctx, "shop", "feat-x"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(f.fetched) != 1 {
		t.Fatalf("%d audit rows, want 1", len(f.fetched))
	}
	row := f.fetched[0]
	if row.Machine != "m1" {
		t.Errorf("the row names %q, want the member m1", row.Machine)
	}
	if row.App != "shop" || row.Env != "feat-x" {
		t.Errorf("the row is about %s/%s", row.App, row.Env)
	}
	if row.Reason != "up" {
		t.Errorf("reason = %q, want what the context said", row.Reason)
	}
	if strings.Join(row.Names, ",") != "GREETING" {
		t.Errorf("names = %v, want the one delivered", row.Names)
	}
	if row.Fingerprint == "" || row.At.IsZero() {
		t.Errorf("row = %+v, want a fingerprint and a time", row)
	}

	if strings.Contains(strings.Join(row.Names, " "), "fleet") {
		t.Error("the audit row carries a value")
	}
}

func TestAMemberStoresNothingItFetched(t *testing.T) {
	f := newFleet(t)
	f.set(EnvRef("shop", "feat-x", "GREETING"), "fleet")

	for i := 0; i < 3; i++ {
		if _, err := f.member.Resolve(context.Background(), "shop", "feat-x"); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}
	if len(f.memberRows.entries) != 0 {
		t.Fatalf("the member's own vault holds %d rows after three fetches, want none",
			len(f.memberRows.entries))
	}

	if len(f.fetched) != 3 {
		t.Errorf("%d audit rows for three fetches, want 3", len(f.fetched))
	}
}

func TestAMemberSendsTheHubBackToWhoeverTypedSecretsAtIt(t *testing.T) {
	f := newFleet(t)
	ctx := context.Background()
	cases := map[string]error{}
	_, err := f.member.List(ctx, "shop", "feat-x")
	cases["list"] = err
	_, err = f.member.Get(ctx, AppRef("shop", "STRIPE_KEY"))
	cases["get"] = err
	_, err = f.member.Set(ctx, AppRef("shop", "STRIPE_KEY"), "sk_live_42")
	cases["set"] = err
	cases["rm"] = f.member.Remove(ctx, EnvRef("shop", "feat-x", "GREETING"))

	for what, err := range cases {
		if !errors.Is(err, ErrHub) {
			t.Errorf("%s on a member: %v, want ErrHub", what, err)
		}
		if err == nil || !strings.Contains(err.Error(), "hub1") {
			t.Errorf("%s on a member: %v, want the hub named", what, err)
		}
	}

	if len(f.fetched) != 0 {
		t.Errorf("%d bundles were fetched by a refusal", len(f.fetched))
	}
}

func TestAMemberWhoseHubIsGoneSaysSoNamingIt(t *testing.T) {
	member := &Remote{Hub: "hub1", Machine: "m1", Fetch: brokenHub{}}
	_, err := member.Resolve(context.Background(), "shop", "feat-x")
	if err == nil {
		t.Fatal("Resolve with an unreachable hub: no error")
	}
	if !strings.Contains(err.Error(), "hub1") || !strings.Contains(err.Error(), "feat-x") {
		t.Errorf("error %q does not name the hub and the environment", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not carry what actually failed", err)
	}

	orphan := &Remote{Hub: "hub1", Machine: "m1"}
	if _, err := orphan.Fingerprint(context.Background(), "shop", "feat-x"); !errors.Is(err, ErrHub) {
		t.Errorf("Fingerprint with no fetcher: %v, want ErrHub", err)
	}
}

func TestTheFingerprintTravelsWithTheBundle(t *testing.T) {
	f := newFleet(t)
	f.set(EnvRef("shop", "feat-x", "GREETING"), "fleet")

	first, err := f.member.Fingerprint(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	want, err := f.hubDB.Fingerprint(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if first != want || first == "" {
		t.Fatalf("the member's fingerprint is %q, the hub's %q", first, want)
	}

	f.set(EnvRef("shop", "feat-x", "GREETING"), "fleet again")
	second, err := f.member.Fingerprint(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("the fingerprint did not move when the value did")
	}
}

func TestAnEnvironmentWithNoSecretsBundlesToNothingAndNotToAnError(t *testing.T) {
	f := newFleet(t)
	values, err := f.member.Resolve(context.Background(), "shop", "feat-x")
	if err != nil {
		t.Fatalf("Resolve of an environment with no secrets: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("resolved %v, want nothing", values)
	}
	if len(f.fetched) != 1 || len(f.fetched[0].Names) != 0 {
		t.Errorf("audit = %+v, want one row naming no secret", f.fetched)
	}
}

func TestTheBundleSaysWhichHubAnsweredAndWhen(t *testing.T) {
	f := newFleet(t)
	f.set(AppRef("shop", "STRIPE_KEY"), "sk_live_42")

	b, err := f.hub.Bundle(context.Background(), BundleRequest{App: "shop", Env: "feat-x", Machine: "m1"})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if b.Machine != "hub1" {
		t.Errorf("the bundle says %q answered, want hub1", b.Machine)
	}
	if b.FetchedAt.IsZero() {
		t.Error("the bundle has no time on it")
	}

	red := b.Redacted()
	if red.Values["STRIPE_KEY"] != Redacted {
		t.Errorf("the redacted bundle carries %q", red.Values["STRIPE_KEY"])
	}
	if b.Values["STRIPE_KEY"] != "sk_live_42" {
		t.Error("Redacted changed the bundle it was called on")
	}
}

func TestTheHubRefusesABundleNobodyCanBeAttributedTo(t *testing.T) {
	f := newFleet(t)
	_, err := f.hub.Bundle(context.Background(), BundleRequest{App: "shop", Env: "feat-x"})
	if err == nil {
		t.Fatal("a bundle was served to nobody in particular")
	}
	if !strings.Contains(err.Error(), "which machine") {
		t.Errorf("error %q does not say what is missing", err)
	}

	f.hub.Asker = func(ctx context.Context) string { return "m2" }
	if _, err := f.hub.Bundle(context.Background(), BundleRequest{App: "shop", Env: "feat-x"}); err != nil {
		t.Fatalf("Bundle with an identity on the session: %v", err)
	}
	if len(f.fetched) != 1 || f.fetched[0].Machine != "m2" {
		t.Errorf("audit = %+v, want it to name m2", f.fetched)
	}
}

func TestTheHubRefusesWhatItCannotAnswer(t *testing.T) {
	f := newFleet(t)
	for name, req := range map[string]BundleRequest{
		"no app": {Env: "feat-x", Machine: "m1"},
		"no env": {App: "shop", Machine: "m1"},
	} {
		if _, err := f.hub.Bundle(context.Background(), req); err == nil {
			t.Errorf("Bundle with %s: no error", name)
		}
	}
	none := &Hub{Machine: "hub1"}
	if _, err := none.Bundle(context.Background(),
		BundleRequest{App: "shop", Env: "feat-x", Machine: "m1"}); !errors.Is(err, ErrNoKey) {
		t.Errorf("Bundle from a machine with no vault: %v, want ErrNoKey", err)
	}
}

func TestAReasonIsWhatTheContextSaidOrWhatWasAsked(t *testing.T) {
	f := newFleet(t)
	if _, err := f.member.Resolve(context.Background(), "shop", "feat-x"); err != nil {
		t.Fatal(err)
	}
	if f.fetched[0].Reason != "resolve" {
		t.Errorf("reason = %q, want the method's own name when nobody said", f.fetched[0].Reason)
	}
	if _, err := f.member.Resolve(WithReason(context.Background(), "deploy"), "shop", "feat-x"); err != nil {
		t.Fatal(err)
	}
	if f.fetched[1].Reason != "deploy" {
		t.Errorf("reason = %q, want the context's", f.fetched[1].Reason)
	}

	ctx := WithReason(WithReason(context.Background(), "deploy"), "  ")
	if got := ReasonFrom(ctx); got != "deploy" {
		t.Errorf("ReasonFrom = %q, want deploy", got)
	}
}

type brokenHub struct{}

func (brokenHub) Bundle(context.Context, BundleRequest) (*Bundle, error) {
	return nil, errors.New("dial 10.86.0.1:4022: connection refused")
}
