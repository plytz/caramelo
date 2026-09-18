package vpnclient

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/state"
)

func testRecord(machine string) Record {
	return Record{
		Fleet:       machine,
		MachineName: "worker1",
		Endpoint:    "192.168.56.11:4021",
		MachineKey:  "0000000000000000000000000000000000000000000=",
		Subnet:      netip.MustParsePrefix("10.86.0.0/16"),
		MachineIP:   netip.MustParseAddr("10.86.0.1"),
		PeerName:    "alex-laptop",
		IP:          netip.MustParseAddr("10.86.0.2"),
		PublicKey:   "1111111111111111111111111111111111111111111=",
		APIPort:     4022,
		UpdatedAt:   time.Now().UTC().Truncate(time.Second),
	}
}

func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rs := &FileRecordStore{Dir: dir}
	rec := testRecord("worker1")
	if err := rs.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := rs.Load("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != rec.Endpoint || got.IP != rec.IP || got.Subnet != rec.Subnet {
		t.Fatalf("Load = %+v, want %+v", got, rec)
	}
	fi, err := os.Stat(filepath.Join(dir, "worker1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("the record is %o, want 600", mode)
	}

	b, err := os.ReadFile(filepath.Join(dir, "worker1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private") {
		t.Errorf("the record mentions a private key:\n%s", b)
	}
}

func TestRecordDerivedAddresses(t *testing.T) {
	rec := testRecord("worker1")
	if got, want := rec.Resolver(), "10.86.0.1:53"; got != want {
		t.Errorf("Resolver = %q, want %q", got, want)
	}
	if got, want := rec.APIAddr(), "10.86.0.1:4022"; got != want {
		t.Errorf("APIAddr = %q, want %q", got, want)
	}
	if got, want := rec.Host(), "192.168.56.11"; got != want {
		t.Errorf("Host = %q, want %q", got, want)
	}
	rec.APIPort = 0
	if got, want := rec.APIAddr(), "10.86.0.1:4022"; got != want {
		t.Errorf("APIAddr with no port = %q, want the default %q", got, want)
	}
	if (Record{}).Valid() {
		t.Error("an empty record claims to be usable")
	}
}

func TestRecordListAndRemove(t *testing.T) {
	dir := t.TempDir()
	rs := &FileRecordStore{Dir: dir}
	for _, m := range []string{"worker1", "worker2"} {
		if err := rs.Save(testRecord(m)); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "worker1.key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := rs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d records, want 2: %+v", len(all), all)
	}
	if err := rs.Remove("worker1"); err != nil {
		t.Fatal(err)
	}
	if err := rs.Remove("worker1"); err != nil {
		t.Fatalf("removing a record twice: %v", err)
	}
	if _, err := rs.Load("worker1"); err == nil {
		t.Fatal("the record survived Remove")
	}
}

func TestRecordPathIsOnePathElement(t *testing.T) {
	rs := &FileRecordStore{Dir: "/tmp/x"}
	got := rs.Path("../../etc/passwd")
	if filepath.Dir(got) != "/tmp/x" {
		t.Fatalf("Path escaped the directory: %q", got)
	}
	if !strings.HasSuffix(got, ".json") {
		t.Fatalf("Path = %q, want a .json file", got)
	}
}

func TestFindRecordMatchesEveryNameForTheMachine(t *testing.T) {
	dir := t.TempDir()
	rs := &FileRecordStore{Dir: dir}
	if err := rs.Save(testRecord("box")); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"box", "worker1", "192.168.56.11", "worker1.internal", "10.86.0.1"} {
		if _, err := findRecord(rs, host); err != nil {
			t.Errorf("findRecord(%q): %v", host, err)
		}
	}
	if _, err := findRecord(rs, "somewhere-else"); err == nil {
		t.Error("findRecord matched a machine the commander has never joined")
	} else if !strings.Contains(err.Error(), remote.ErrNoTunnel.Error()) {
		t.Errorf("err = %v, want ErrNoTunnel so the caller falls through to the next transport", err)
	}
}

func TestCheckRoutableRefusesARecordThatIsNotAMachinesRange(t *testing.T) {
	ok := testRecord("worker1")
	if err := ok.CheckRoutable(); err != nil {
		t.Fatalf("a normal record was refused: %v", err)
	}
	for _, tc := range []struct {
		what string
		make func(Record) Record
		want string
	}{
		{"a default route", func(r Record) Record {
			r.Subnet = netip.MustParsePrefix("0.0.0.0/0")
			return r
		}, "larger than any machine's range"},
		{"a public range", func(r Record) Record {
			r.Subnet = netip.MustParsePrefix("8.8.0.0/16")
			r.MachineIP = netip.MustParseAddr("8.8.0.1")
			r.IP = netip.MustParseAddr("8.8.0.2")
			return r
		}, "not a private range"},
		{"an address outside the range", func(r Record) Record {
			r.IP = netip.MustParseAddr("192.168.1.5")
			return r
		}, "outside its own range"},
		{"the machine outside the range", func(r Record) Record {
			r.MachineIP = netip.MustParseAddr("172.16.0.1")
			return r
		}, "outside its own range"},
		{"no endpoint", func(r Record) Record {
			r.Endpoint = ""
			return r
		}, "incomplete"},
	} {
		err := tc.make(testRecord("worker1")).CheckRoutable()
		if err == nil {
			t.Errorf("%s was accepted", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", tc.what, err, tc.want)
		}
	}
}

func TestARecordRoutesTheFleetToAHubThatRelays(t *testing.T) {
	st := &api.Status{
		Hostname: "hub",
		VPN: &api.VPNStatus{
			Enabled: true, PublicKey: "0000000000000000000000000000000000000000000=", Endpoint: "hub.example.com:4021",
			Subnet: "10.86.0.0/16", Address: "10.86.0.1", Reach: "10.80.0.0/12",
		},
	}
	peer := &state.Peer{Name: "laptop", IP: "10.86.0.2"}
	rec, err := RecordFrom("hub", "laptop", "0000000000000000000000000000000000000000000=", st, peer)
	if err != nil {
		t.Fatalf("RecordFrom: %v", err)
	}
	if got := rec.Subnet.String(); got != "10.80.0.0/12" {
		t.Errorf("the record routes %s to the hub, want the fleet's range", got)
	}
	if err := rec.CheckRoutable(); err != nil {
		t.Errorf("CheckRoutable: %v", err)
	}

	st.VPN.Reach = ""
	rec, err = RecordFrom("hub", "laptop", "0000000000000000000000000000000000000000000=", st, peer)
	if err != nil {
		t.Fatalf("RecordFrom: %v", err)
	}
	if got := rec.Subnet.String(); got != "10.86.0.0/16" {
		t.Errorf("a machine of one routes %s, want its own range", got)
	}
}
