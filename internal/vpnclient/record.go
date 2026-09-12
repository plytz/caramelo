package vpnclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/plytz/caramelo/internal/userdir"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/vpn"
)

const RecordExt = ".json"

type Record struct {
	Machine string `json:"machine"`

	MachineName string `json:"machine_name,omitempty"`

	Endpoint string `json:"endpoint"`

	MachineKey string `json:"machine_key"`

	Subnet    netip.Prefix `json:"subnet"`
	MachineIP netip.Addr   `json:"machine_ip"`

	PeerName  string     `json:"peer_name"`
	IP        netip.Addr `json:"ip"`
	PublicKey string     `json:"public_key"`

	APIPort int `json:"api_port"`

	LastHandshake time.Time `json:"last_handshake,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

func (r Record) Resolver() string {
	if !r.MachineIP.IsValid() {
		return ""
	}
	return netip.AddrPortFrom(r.MachineIP, vpn.ResolverPort).String()
}

func (r Record) APIAddr() string {
	if !r.MachineIP.IsValid() {
		return ""
	}
	port := r.APIPort
	if port == 0 {
		port = defaultAPIPort
	}
	return netip.AddrPortFrom(r.MachineIP, uint16(port)).String()
}

func (r Record) Host() string {
	if r.Endpoint == "" {
		return ""
	}
	if ap, err := netip.ParseAddrPort(r.Endpoint); err == nil {
		return ap.Addr().String()
	}
	if i := strings.LastIndex(r.Endpoint, ":"); i > 0 {
		return r.Endpoint[:i]
	}
	return r.Endpoint
}

func (r Record) Valid() bool {
	return r.Endpoint != "" && r.MachineKey != "" && r.IP.IsValid() && r.MachineIP.IsValid()
}

const minSubnetBits = 8

func (r Record) CheckRoutable() error {
	if !r.Valid() {
		return fmt.Errorf("the record for %s is incomplete: it needs an endpoint, "+
			"the machine's public key, and both addresses", r.Machine)
	}
	subnet, err := vpn.Subnet(r.Subnet.String())
	if err != nil {
		return fmt.Errorf("the record for %s: %w", r.Machine, err)
	}
	if subnet.Bits() < minSubnetBits {
		return fmt.Errorf("the record for %s claims %s, which is far larger than any machine's "+
			"range: refusing to route it", r.Machine, subnet)
	}
	if !subnet.Addr().IsPrivate() {
		return fmt.Errorf("the record for %s claims %s, which is not a private range: "+
			"refusing to route it", r.Machine, subnet)
	}
	for _, a := range []struct {
		what string
		ip   netip.Addr
	}{{"this computer's address", r.IP}, {"the machine's address", r.MachineIP}} {
		if !subnet.Contains(a.ip) {
			return fmt.Errorf("the record for %s puts %s (%s) outside its own range %s",
				r.Machine, a.what, a.ip, subnet)
		}
	}
	return nil
}

type RecordStore interface {
	Load(machine string) (Record, error)
	Save(r Record) error
	Remove(machine string) error
	List() ([]Record, error)
	Path(machine string) string
}

type FileRecordStore struct {
	Dir string
}

var _ RecordStore = (*FileRecordStore)(nil)

func (s *FileRecordStore) Path(machine string) string {
	name := strings.TrimSuffix(KeyFileName(machine), ".key") + RecordExt
	if s.Dir != "" {
		return filepath.Join(s.Dir, name)
	}
	dir, err := userdir.Config()
	if err != nil {
		return filepath.Join("caramelo", KeyDir, name)
	}
	return filepath.Join(dir, "caramelo", KeyDir, name)
}

func (s *FileRecordStore) Load(machine string) (Record, error) {
	path := s.Path(machine)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%s: %w", machine, ErrNoKey)
	}
	if err != nil {
		return Record{}, fmt.Errorf("read %s: %w", path, err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Machine == "" {
		r.Machine = machine
	}
	return r, nil
}

func (s *FileRecordStore) Save(r Record) error {
	if r.Machine == "" {
		return errors.New("a machine record needs a machine name")
	}
	path := s.Path(r.Machine)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record for %s: %w", r.Machine, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func (s *FileRecordStore) Remove(machine string) error {
	path := s.Path(machine)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func (s *FileRecordStore) List() ([]Record, error) {
	dir := filepath.Dir(s.Path("machine"))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), RecordExt) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil || r.Machine == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
