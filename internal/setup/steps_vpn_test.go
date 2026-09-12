package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
	"github.com/plytz/caramelo/internal/vpn"
)

const keyPath = "/var/lib/caramelo/vpn/private.key"

func sampleKey(t *testing.T) vpn.Key {
	t.Helper()
	k, err := vpn.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVPNCheckOnABareBox(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	env, _ := testEnv(t, run)

	done, detail, err := (&VPNStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done || !strings.Contains(detail, keyPath+" missing") {
		t.Fatalf("Check() = %v, %q; want the missing key", done, detail)
	}
}

func TestVPNApplyGeneratesTheKey(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("cat -- ", 1)
	env, log := testEnv(t, run)

	if err := (&VPNStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	if !run.Ran("mkdir -p -- /var/lib/caramelo/vpn") {
		t.Errorf("the key directory was not created:\n%s", run.Transcript())
	}
	if !run.Ran("chmod 0700 -- /var/lib/caramelo/vpn") {
		t.Errorf("the key directory is not private:\n%s", run.Transcript())
	}

	var written string
	for _, c := range run.Calls() {
		if c.Line == "tee -- "+keyPath {
			written = c.Stdin
			if c.Cmd.User != "caramelo" {
				t.Errorf("the key was written as %q, want the daemon's user", c.Cmd.User)
			}
		}
	}
	if written == "" {
		t.Fatalf("the key was never written:\n%s", run.Transcript())
	}
	key, err := vpn.ParseKey(written)
	if err != nil {
		t.Fatalf("what was written is not a key: %v", err)
	}
	if key.IsZero() {
		t.Fatal("the generated key is empty")
	}
	if !run.Ran("chmod 0600 -- " + keyPath) {
		t.Errorf("the key is not 0600:\n%s", run.Transcript())
	}
	if !run.Ran("chown caramelo:caramelo -- " + keyPath) {
		t.Errorf("the key does not belong to the daemon:\n%s", run.Transcript())
	}

	pub, err := key.Public()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), pub.Base64()) {
		t.Errorf("the public key was not reported:\n%s", log.String())
	}
	if strings.Contains(log.String(), key.Base64()) {
		t.Error("the private key was printed")
	}
}

func TestVPNApplyNeverReplacesAnExistingKey(t *testing.T) {
	run := testutil.New()
	key := sampleKey(t)
	run.Stdout("cat -- "+keyPath, key.Base64()+"\n")
	env, _ := testEnv(t, run)

	if err := (&VPNStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	if run.Ran("tee -- " + keyPath) {
		t.Errorf("the key was rewritten:\n%s", run.Transcript())
	}

	if !run.Ran("chmod 0600 -- "+keyPath) || !run.Ran("chown caramelo:caramelo -- "+keyPath) {
		t.Errorf("the key's owner and mode were not corrected:\n%s", run.Transcript())
	}
}

func TestVPNCheckIsDoneOnAFinishedBox(t *testing.T) {
	run := testutil.New()
	key := sampleKey(t)
	run.Stdout("stat -c %U:%G:%a:%F -- "+keyPath, "caramelo:caramelo:600:regular file\n")
	run.Stdout("cat -- "+keyPath, key.Base64()+"\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&VPNStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	pub, _ := key.Public()
	for _, want := range []string{serverconfig.DefaultVPNSubnet, serverconfig.DefaultVPNListen, pub.Base64()} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
}

func TestVPNCheckNoticesAReadableKey(t *testing.T) {
	run := testutil.New()
	key := sampleKey(t)
	run.Stdout("stat -c %U:%G:%a:%F -- "+keyPath, "caramelo:caramelo:644:regular file\n")
	run.Stdout("cat -- "+keyPath, key.Base64()+"\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&VPNStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if done || !strings.Contains(detail, "mode 0644") {
		t.Fatalf("Check() = %v, %q; a world-readable private key must be a difference", done, detail)
	}
}

func peerEnv(t *testing.T, run *testutil.FakeRunner, peer PeerSpec) *Env {
	t.Helper()
	env, _ := testEnv(t, run)
	env.Opts.Peer = peer
	return env
}

func samplePeer(t *testing.T) PeerSpec {
	t.Helper()
	pub, err := sampleKey(t).Public()
	if err != nil {
		t.Fatal(err)
	}
	return PeerSpec{Name: "laptop", PublicKey: pub.Base64()}
}

func TestPeerStepIsSkippedWithoutTheFlag(t *testing.T) {
	run := testutil.New()
	env, _ := testEnv(t, run)
	_, _, err := (&PeerStep{}).Check(context.Background(), env)
	var skip Skip
	if !asSkip(err, &skip) {
		t.Fatalf("err = %v, want Skip", err)
	}
	if len(run.Calls()) != 0 {
		t.Errorf("the box was touched anyway:\n%s", run.Transcript())
	}
}

func TestPeerStepAdmitsTheIdentity(t *testing.T) {
	run := testutil.New()
	peer := samplePeer(t)
	list := serverconfig.BinaryPath + " peer list --json"
	run.Stdout(list, "[]\n")
	run.Stdout(serverconfig.BinaryPath+" peer add "+peer.Name+" "+peer.PublicKey+" --json",
		`{"name":"laptop","public_key":"`+peer.PublicKey+`","ip":"10.86.0.2"}`+"\n")
	env := peerEnv(t, run, peer)

	done, detail, err := (&PeerStep{}).Check(context.Background(), env)
	if err != nil || done {
		t.Fatalf("Check() = %v, %q, %v; want not done", done, detail, err)
	}
	if err := (&PeerStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	for _, c := range run.Calls() {
		if strings.HasPrefix(c.Line, serverconfig.BinaryPath+" peer add") && c.Cmd.User != "caramelo" {
			t.Errorf("peer add ran as %q, want the daemon's user", c.Cmd.User)
		}
	}
}

func TestPeerStepIsDoneWhenTheIdentityIsAlreadyThere(t *testing.T) {
	run := testutil.New()
	peer := samplePeer(t)
	run.Stdout(serverconfig.BinaryPath+" peer list --json",
		`[{"name":"laptop","public_key":"`+peer.PublicKey+`","ip":"10.86.0.2"}]`+"\n")
	env := peerEnv(t, run, peer)

	done, detail, err := (&PeerStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	if !strings.Contains(detail, "10.86.0.2") {
		t.Errorf("detail %q does not say where the peer is", detail)
	}
}

func TestPeerStepNoticesADifferentKeyUnderTheSameName(t *testing.T) {
	run := testutil.New()
	peer := samplePeer(t)
	other := samplePeer(t)
	run.Stdout(serverconfig.BinaryPath+" peer list --json",
		`[{"name":"laptop","public_key":"`+other.PublicKey+`","ip":"10.86.0.2"}]`+"\n")
	env := peerEnv(t, run, peer)

	done, detail, err := (&PeerStep{}).Check(context.Background(), env)
	if err != nil || done {
		t.Fatalf("Check() = %v, %q, %v; want a difference", done, detail, err)
	}
	if !strings.Contains(detail, "different key") {
		t.Errorf("detail = %q", detail)
	}
}

func TestPeerStepSaysSoWhenTheDaemonIsNotAnswering(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix(serverconfig.BinaryPath, 1)
	env := peerEnv(t, run, samplePeer(t))

	done, detail, err := (&PeerStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("a daemon that is not up yet is a difference, not a failure: %v", err)
	}
	if done || !strings.Contains(detail, "not answering") {
		t.Fatalf("Check() = %v, %q", done, detail)
	}
}

func TestPeerStepRefusesAKeyThatIsNotOne(t *testing.T) {
	run := testutil.New()
	env := peerEnv(t, run, PeerSpec{Name: "laptop", PublicKey: "nonsense"})
	if _, _, err := (&PeerStep{}).Check(context.Background(), env); err == nil {
		t.Fatal("a value that is not a key was accepted")
	}
}

func TestParsePeer(t *testing.T) {
	key := samplePeer(t).PublicKey
	for _, in := range []string{
		"laptop " + key,
		"laptop=" + key,
		"laptop," + key,
		"  laptop\t" + key + "  ",
	} {
		got, err := ParsePeer(in)
		if err != nil {
			t.Fatalf("ParsePeer(%q): %v", in, err)
		}

		if got.Name != "laptop" || got.PublicKey != key {
			t.Errorf("ParsePeer(%q) = %+v", in, got)
		}
	}
	if got, err := ParsePeer(""); err != nil || !got.Empty() {
		t.Errorf("ParsePeer(\"\") = %+v, %v; want the zero spec", got, err)
	}
	for _, bad := range []string{"laptop", "laptop notakey", " " + key, "laptop "} {
		if _, err := ParsePeer(bad); err == nil {
			t.Errorf("ParsePeer(%q) was accepted", bad)
		}
	}
}

func asSkip(err error, out *Skip) bool {
	s, ok := err.(Skip)
	if ok {
		*out = s
	}
	return ok
}
