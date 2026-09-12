package vpn

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type ipcConfig struct {
	PrivateKey *Key

	ListenPort *int

	ReplacePeers bool
	Peers        []ipcPeer
}

type ipcPeer struct {
	PublicKey Key

	Remove bool

	ReplaceAllowedIPs bool

	AllowedIPs []netip.Prefix

	Endpoint string

	Keepalive int
}

func (c ipcConfig) String() string {
	var b strings.Builder
	if c.PrivateKey != nil {
		fmt.Fprintf(&b, "private_key=%s\n", c.PrivateKey.Hex())
	}
	if c.ListenPort != nil {
		fmt.Fprintf(&b, "listen_port=%d\n", *c.ListenPort)
	}
	if c.ReplacePeers {
		b.WriteString("replace_peers=true\n")
	}
	for _, p := range c.Peers {
		fmt.Fprintf(&b, "public_key=%s\n", p.PublicKey.Hex())
		if p.Remove {
			b.WriteString("remove=true\n")
			continue
		}
		if p.ReplaceAllowedIPs {
			b.WriteString("replace_allowed_ips=true\n")
		}
		if p.Endpoint != "" {
			fmt.Fprintf(&b, "endpoint=%s\n", p.Endpoint)
		}
		for _, a := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", a.String())
		}
		if p.Keepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.Keepalive)
		}
	}
	return b.String()
}

type ipcStatus struct {
	ListenPort int

	Peers map[string]ipcPeerStatus
}

type ipcPeerStatus struct {
	LastHandshake time.Time
	RxBytes       uint64
	TxBytes       uint64
	Endpoint      string
}

func parseIPCStatus(r io.Reader) (ipcStatus, error) {
	st := ipcStatus{Peers: map[string]ipcPeerStatus{}}
	var (
		cur     string
		curPeer ipcPeerStatus
		sec     int64
		nsec    int64
	)
	flush := func() {
		if cur == "" {
			return
		}
		if sec != 0 || nsec != 0 {
			curPeer.LastHandshake = time.Unix(sec, nsec)
		}
		st.Peers[cur] = curPeer
		cur, curPeer, sec, nsec = "", ipcPeerStatus{}, 0, 0
	}
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "public_key":
			flush()
			cur = v
		case "listen_port":
			n, err := strconv.Atoi(v)
			if err != nil {
				return st, fmt.Errorf("vpn: parse device status: listen_port %q: %w", v, err)
			}
			st.ListenPort = n
		case "endpoint":
			curPeer.Endpoint = v
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(v, 10, 64)
		case "rx_bytes":
			curPeer.RxBytes, _ = strconv.ParseUint(v, 10, 64)
		case "tx_bytes":
			curPeer.TxBytes, _ = strconv.ParseUint(v, 10, 64)
		case "errno":
			if v != "0" {
				return st, fmt.Errorf("vpn: read device status: errno %s", v)
			}
		}
	}
	flush()
	if err := s.Err(); err != nil {
		return st, fmt.Errorf("vpn: read device status: %w", err)
	}
	return st, nil
}
