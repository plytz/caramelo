package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

type VPNStep struct{}

func NewVPNStep() *VPNStep { return &VPNStep{} }

func (s *VPNStep) Name() string { return "vpn" }

func (s *VPNStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	cfg := env.Config
	path := cfg.VPNKeyPath()
	st, err := statPath(ctx, env, path)
	if err != nil {
		return false, "", err
	}
	switch {
	case !st.Exists:
		return false, path + " missing", nil
	case st.Owner != cfg.User || st.Group != cfg.Group:
		return false, fmt.Sprintf("%s owned by %s:%s, want %s:%s", path, st.Owner, st.Group, cfg.User, cfg.Group), nil
	case st.Mode != keyMode:
		return false, fmt.Sprintf("%s mode %s, want %s", path, st.Mode, keyMode), nil
	}
	pub, err := s.publicKey(ctx, env)
	if err != nil {
		return false, path + " is not a key", nil
	}
	return true, fmt.Sprintf("%s on %s, public key %s", cfg.VPNSubnet, cfg.VPNListen, pub), nil
}

const keyMode = "0600"

func (s *VPNStep) Apply(ctx context.Context, env *Env) error {
	cfg := env.Config
	path := cfg.VPNKeyPath()

	if _, err := s.publicKey(ctx, env); err == nil {
		return s.own(ctx, env, path)
	}

	dir := dirSpec{Path: cfg.VPNDir(), Mode: "0700", Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User}
	if err := dir.apply(ctx, env); err != nil {
		return fmt.Errorf("create %s: %w", cfg.VPNDir(), err)
	}
	key, err := vpn.GenerateKey()
	if err != nil {
		return err
	}

	spec := fileSpec{Path: path, Content: key.Base64() + "\n", Mode: keyMode,
		Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User}
	if err := spec.apply(ctx, env); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	pub, err := key.Public()
	if err != nil {
		return err
	}
	logf(env, "generated the machine's network key, public key %s", pub.Base64())
	return nil
}

func (s *VPNStep) own(ctx context.Context, env *Env, path string) error {
	cfg := env.Config
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown", Args: []string{cfg.User + ":" + cfg.Group, "--", path}}); err != nil {
		return err
	}
	_, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{keyMode, "--", path}})
	return err
}

func (s *VPNStep) publicKey(ctx context.Context, env *Env) (string, error) {
	content, exists, err := readFile(ctx, env, env.Config.VPNKeyPath())
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("no key at %s", env.Config.VPNKeyPath())
	}
	key, err := vpn.ParseKey(content)
	if err != nil {
		return "", err
	}
	if key.IsZero() {
		return "", fmt.Errorf("the key at %s is empty", env.Config.VPNKeyPath())
	}
	pub, err := key.Public()
	if err != nil {
		return "", err
	}
	return pub.Base64(), nil
}

type PeerStep struct{}

func NewPeerStep() *PeerStep { return &PeerStep{} }

func (s *PeerStep) Name() string { return "peer" }

func (s *PeerStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	peer := env.Opts.Peer
	if peer.Empty() {
		return false, "", Skip{Reason: "no --peer"}
	}
	if err := peer.Validate(); err != nil {
		return false, "", err
	}
	peers, err := s.list(ctx, env)
	if err != nil {

		return false, "caramelod is not answering yet", nil
	}
	for _, p := range peers {
		if p.Name != peer.Name {
			continue
		}
		if p.PublicKey != peer.PublicKey {
			return false, fmt.Sprintf("peer %s has a different key", peer.Name), nil
		}
		return true, fmt.Sprintf("%s on %s", p.Name, p.IP), nil
	}
	return false, "peer " + peer.Name + " is not admitted yet", nil
}

func (s *PeerStep) Apply(ctx context.Context, env *Env) error {
	peer := env.Opts.Peer
	out, err := mustRun(ctx, env, s.caramelo(env, "peer", "add", peer.Name, peer.PublicKey, "--json"))
	if err != nil {
		return fmt.Errorf("admit peer %s: %w", peer.Name, err)
	}
	var added peerLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &added); err != nil {
		return fmt.Errorf("admit peer %s: unexpected answer %q", peer.Name, out)
	}
	logf(env, "admitted peer %s on %s", added.Name, added.IP)
	return nil
}

type peerLine struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	IP        string `json:"ip"`
}

func (s *PeerStep) list(ctx context.Context, env *Env) ([]peerLine, error) {
	out, err := mustRun(ctx, env, s.caramelo(env, "peer", "list", "--json"))
	if err != nil {
		return nil, err
	}
	var peers []peerLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &peers); err != nil {
		return nil, fmt.Errorf("read the peer list: %w", err)
	}
	return peers, nil
}

func (s *PeerStep) caramelo(env *Env, args ...string) runner.Cmd {
	return runner.Cmd{Name: serverconfig.BinaryPath, Args: args, User: env.Config.User}
}
