//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/state"
	"github.com/plytz/caramelo/internal/vpnclient"
)

const (
	LabPeerName    = "lab-client"
	labPeerKeyFile = "lab-client"
	VPNPort        = 4021
)

func LabPeerKey() (vpnclient.KeyPair, error) {
	dir, err := SharedCacheDir("vpn")
	if err != nil {
		return vpnclient.KeyPair{}, err
	}
	lock, err := lockCache("vpn-key")
	if err != nil {
		return vpnclient.KeyPair{}, err
	}
	defer lock.release()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return vpnclient.KeyPair{}, fmt.Errorf("create %s: %w", dir, err)
	}
	store := &vpnclient.FileKeyStore{Dir: dir}
	kp, _, err := store.Ensure(labPeerKeyFile)
	if err != nil {
		return vpnclient.KeyPair{}, fmt.Errorf("the lab identity's key: %w", err)
	}
	return kp, nil
}

func JoinLabPeer(home string, m *Machine) error {
	kp, err := LabPeerKey()
	if err != nil {
		return err
	}
	endpoint, err := m.HostAddrProto(VPNPort, "udp")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var peer state.Peer
	if err := machineJSON(ctx, m, &peer, "peer", "add", LabPeerName, kp.Public, "--json"); err != nil {
		return fmt.Errorf("admit %s on %s: %w", LabPeerName, m.Alias, err)
	}
	var st api.Status
	if err := machineJSON(ctx, m, &st, "status", "--json"); err != nil {
		return fmt.Errorf("read the network of %s: %w", m.Alias, err)
	}
	if st.VPN == nil || !st.VPN.Enabled {
		return fmt.Errorf("%s reports no working network: %+v", m.Alias, st.VPN)
	}
	rec, err := vpnclient.RecordFrom(m.HostIP(), LabPeerName, kp.Public, &st, &peer)
	if err != nil {
		return err
	}
	rec.Endpoint = endpoint
	rec.UpdatedAt = time.Now().UTC()

	dir := filepath.Join(home, ".config", "caramelo", vpnclient.KeyDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	keyPath := filepath.Join(dir, vpnclient.KeyFileName(rec.Machine))
	if err := os.WriteFile(keyPath, []byte(kp.Private+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := (&vpnclient.FileRecordStore{Dir: dir}).Save(rec); err != nil {
		return fmt.Errorf("write the machine record for %s: %w", m.Alias, err)
	}
	return nil
}

func InstallBinaryOn(ctx context.Context, m *Machine) error {
	bin, err := BinaryFor(ctx, m)
	if err != nil {
		return err
	}
	same, err := sameFileOn(ctx, m, bin, RemoteBin)
	if err != nil {
		return err
	}
	if !same {
		if err := m.Copy(ctx, bin, RemoteBin); err != nil {
			return err
		}
	}
	res, err := m.RunAsRoot(ctx, "install -m 0755 "+RemoteBin+" "+CarameloBinary)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("install the binary on %s: exit %d: %s", m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func machineJSON(ctx context.Context, m *Machine, v any, args ...string) error {
	cmd := AsUser(m, CarameloUser, CarameloBinary+" "+strings.Join(args, " "))
	res, err := m.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: exit %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), v); err != nil {
		return fmt.Errorf("%s: %w (stdout %q)", cmd, err, res.Stdout)
	}
	return nil
}

func sameFileOn(ctx context.Context, m *Machine, local, remote string) (bool, error) {
	want, err := FileSHA256(local)
	if err != nil {
		return false, err
	}
	res, err := m.Run(ctx, "sha256sum "+remote+" 2>/dev/null | cut -d' ' -f1")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == want, nil
}
