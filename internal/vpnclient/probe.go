package vpnclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/vpn"
)

const (
	ProbeReached = "reached"

	ProbeNoAnswer = "no-answer"

	ProbeUnproven = "unproven"

	ProbeNoRoute = "no-route"
)

type ProbeRequest struct {
	Machine string `json:"machine"`

	Endpoint string `json:"endpoint,omitempty"`

	Key string `json:"key,omitempty"`

	Timeout time.Duration `json:"timeout,omitempty"`
}

type Probe struct {
	Machine   string        `json:"machine"`
	Endpoint  string        `json:"endpoint"`
	Result    string        `json:"result"`
	Admitted  bool          `json:"admitted"`
	Handshake time.Time     `json:"handshake,omitempty"`
	Elapsed   time.Duration `json:"elapsed"`
	Detail    string        `json:"detail"`
}

func (p *Probe) OK() bool { return p != nil && p.Result == ProbeReached }

var (
	probeSubnet     = netip.MustParsePrefix("10.86.0.0/16")
	probeIP         = netip.MustParseAddr("10.86.255.254")
	probeMachineIP  = netip.MustParseAddr("10.86.0.1")
	probeDialTarget = defaultAPIPort
)

func (c *client) Probe(ctx context.Context, req ProbeRequest) (*Probe, error) {
	machine := strings.TrimSpace(req.Machine)
	if machine == "" {
		return nil, errors.New("no machine: pass a name from the commander config, or the machine's host[:port]")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = verifyTimeout
	}

	rec, recErr := c.opts.Records.Load(machine)
	known := recErr == nil && rec.Valid()

	endpoint, err := probeEndpoint(req, rec, known, machine)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.Key)
	if key == "" && known {
		key = rec.MachineKey
	}
	switch {
	case key == "":
		return nil, fmt.Errorf("no public key for %s: pass --key KEY (the machine prints it in "+
			"'caramelo hub status'), or run 'caramelo vpn up --machine %s' once from here", machine, machine)
	case !ValidKey(key):
		return nil, fmt.Errorf("--key %q: that is not a WireGuard public key", key)
	}

	admitted := known && rec.Endpoint == endpoint && rec.MachineKey == key
	probe := &Probe{Machine: machine, Endpoint: endpoint, Admitted: admitted}

	if err := probeRoute(ctx, endpoint); err != nil {
		probe.Result, probe.Detail = ProbeNoRoute, err.Error()
		return probe, nil
	}
	if _, _, err := c.opts.Keys.Ensure(machine); err != nil {
		return nil, err
	}

	start := time.Now()
	hs, err := probeHandshake(ctx, c, probeRecord(machine, rec, endpoint, key, admitted), timeout)
	probe.Elapsed = time.Since(start).Round(time.Millisecond)
	var setup probeSetupError
	if errors.As(err, &setup) {
		return nil, err
	}
	port := portOf(endpoint)
	switch {
	case err == nil && !hs.IsZero():
		probe.Result, probe.Handshake = ProbeReached, hs
		probe.Detail = fmt.Sprintf("a packet from here arrived on udp %s and was answered", port)
	case admitted:
		probe.Result = ProbeNoAnswer
		probe.Detail = fmt.Sprintf("nothing came back within %s and this commander is admitted on %s, "+
			"so udp %s is being dropped between here and there (a firewall on the machine, "+
			"a security group in front of it, or the network between)", timeout, machine, port)
	default:
		probe.Result = ProbeUnproven
		probe.Detail = fmt.Sprintf("nothing came back within %s, and this commander's key is not admitted on %s, "+
			"so silence proves nothing: an unknown key gets no reply at all. Admit it there with "+
			"'caramelo peer add NAME KEY' and probe again", timeout, machine)
	}
	return probe, nil
}

type probeSetupError struct{ err error }

func (e probeSetupError) Error() string { return e.err.Error() }

func (e probeSetupError) Unwrap() error { return e.err }

var probeHandshake = func(ctx context.Context, c *client, rec Record, timeout time.Duration) (time.Time, error) {
	dev, err := c.device(rec)
	if err != nil {
		return time.Time{}, probeSetupError{fmt.Errorf("open a tunnel to %s: %w", rec.Fleet, err)}
	}
	defer dev.release()
	return verify(ctx, dev, timeout)
}

var probeLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func probeRoute(ctx context.Context, endpoint string) error {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	addrs, err := probeLookup(ctx, host)
	if err != nil {
		return fmt.Errorf("%s does not resolve from here: %v", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%s resolves to no address from here", host)
	}
	return nil
}

func probeEndpoint(req ProbeRequest, rec Record, known bool, machine string) (string, error) {
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" && known {
		endpoint = rec.Endpoint
	}
	if endpoint == "" {
		endpoint = machine
	}
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		return endpoint, nil
	}
	if strings.ContainsAny(endpoint, " \t/") {
		return "", fmt.Errorf("%q is not a machine this commander knows, nor a host: pass --endpoint host:port", endpoint)
	}
	return net.JoinHostPort(endpoint, strconv.Itoa(vpn.DefaultListenPort)), nil
}

func probeRecord(machine string, rec Record, endpoint, key string, admitted bool) Record {
	if admitted {
		return rec
	}
	return Record{
		Fleet:      machine,
		Endpoint:   endpoint,
		MachineKey: key,
		Subnet:     probeSubnet,
		IP:         probeIP,
		MachineIP:  probeMachineIP,
		APIPort:    probeDialTarget,
	}
}

func portOf(endpoint string) string {
	if _, port, err := net.SplitHostPort(endpoint); err == nil {
		return port
	}
	return strconv.Itoa(vpn.DefaultListenPort)
}
