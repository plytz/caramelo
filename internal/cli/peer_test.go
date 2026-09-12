package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/state"
)

type peerService struct {
	api.Service

	peers   []state.Peer
	added   [2]string
	removed string
	forced  bool
	err     error
}

func (s *peerService) AddPeer(_ context.Context, name, publicKey string) (*state.Peer, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.added = [2]string{name, publicKey}
	return &state.Peer{Name: name, PublicKey: publicKey, IP: "10.86.0.2", CreatedAt: time.Now()}, nil
}

func (s *peerService) Peers(context.Context) ([]state.Peer, error) { return s.peers, s.err }

func (s *peerService) RemovePeer(_ context.Context, name string, force bool) error {
	if s.err != nil {
		return s.err
	}
	s.removed, s.forced = name, force
	return nil
}

const testPeerKey = "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE="

func TestPeerAdd(t *testing.T) {
	svc := &peerService{}
	code, stdout, _ := runService(t, context.Background(), svc, "peer", "add", "agent-7", testPeerKey)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.added != [2]string{"agent-7", testPeerKey} {
		t.Errorf("service got %v", svc.added)
	}
	if !strings.Contains(stdout, "added peer agent-7 on 10.86.0.2") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestPeerAddJSONIsOnePeer(t *testing.T) {
	svc := &peerService{}
	code, stdout, _ := runService(t, context.Background(), svc, "peer", "add", "agent-7", testPeerKey, "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var p state.Peer
	if err := json.Unmarshal([]byte(stdout), &p); err != nil {
		t.Fatalf("stdout is not one peer: %v (%q)", err, stdout)
	}
	if p.Name != "agent-7" || p.IP != "10.86.0.2" || p.PublicKey != testPeerKey {
		t.Errorf("peer = %+v", p)
	}
}

func TestPeerList(t *testing.T) {
	svc := &peerService{peers: []state.Peer{
		{Name: "agent-7", PublicKey: testPeerKey, IP: "10.86.0.2", AddedBy: "setup",
			LastHandshake: time.Now().Add(-90 * time.Second)},
		{Name: "laptop", PublicKey: testPeerKey, IP: "10.86.0.3"},
	}}
	code, stdout, _ := runService(t, context.Background(), svc, "peer", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"agent-7", "10.86.0.2", "setup", "ago", "laptop", "never"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not mention %q:\n%s", want, stdout)
		}
	}

	if strings.Contains(stdout, testPeerKey) {
		t.Errorf("the table printed a whole key:\n%s", stdout)
	}
}

func TestPeerListEmptyAndJSON(t *testing.T) {
	svc := &peerService{}
	_, stdout, _ := runService(t, context.Background(), svc, "peer", "list")
	if !strings.Contains(stdout, "no peers") {
		t.Errorf("stdout = %q", stdout)
	}

	_, stdout, _ = runService(t, context.Background(), svc, "peer", "list", "--json")
	var peers []state.Peer
	if err := json.Unmarshal([]byte(stdout), &peers); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if peers == nil || len(peers) != 0 {
		t.Errorf("peers = %v, want an empty list", peers)
	}
}

func TestPeerRemove(t *testing.T) {
	svc := &peerService{}
	code, stdout, _ := runService(t, context.Background(), svc, "peer", "remove", "agent-7")
	if code != ExitOK || svc.removed != "agent-7" {
		t.Fatalf("exit = %d, removed = %q", code, svc.removed)
	}
	if !strings.Contains(stdout, "removed peer agent-7") {
		t.Errorf("stdout = %q", stdout)
	}

	svc = &peerService{}
	if code, _, _ := runService(t, context.Background(), svc, "peer", "rm", "agent-7"); code != ExitOK {
		t.Errorf("peer rm: exit = %d", code)
	}
}

func TestPeerErrorsAndArities(t *testing.T) {
	svc := &peerService{err: errors.New("this machine has no private network")}
	code, _, stderr := runService(t, context.Background(), svc, "peer", "add", "agent-7", testPeerKey)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "no private network") {
		t.Errorf("stderr = %q", stderr)
	}
	for _, args := range [][]string{
		{"peer", "add", "agent-7"},
		{"peer", "add", "agent-7", testPeerKey, "extra"},
		{"peer", "remove"},
		{"peer", "list", "extra"},
	} {
		if code, _, _ := runService(t, context.Background(), &peerService{}, args...); code != ExitUsage {
			t.Errorf("caramelo %s: exit = %d, want %d", strings.Join(args, " "), code, ExitUsage)
		}
	}
}

func TestPeerRemoveForce(t *testing.T) {
	svc := &peerService{}
	code, _, stderr := runWithService(t, svc, "peer", "remove", "laptop", "--force")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if svc.removed != "laptop" || !svc.forced {
		t.Errorf("removed %q force=%v, want laptop with force", svc.removed, svc.forced)
	}

	plain := &peerService{}
	if code, _, _ := runWithService(t, plain, "peer", "remove", "laptop"); code != ExitOK {
		t.Fatalf("exit = %d without --force", code)
	}
	if plain.forced {
		t.Error("force was set without the flag")
	}
}
