package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/vpnclient"
)

func scriptedProbe(t *testing.T, answer func(vpnclient.ProbeRequest) (*vpnclient.Probe, error)) *vpnclient.ProbeRequest {
	t.Helper()
	var seen vpnclient.ProbeRequest
	prev := probeMachine
	probeMachine = func(_ context.Context, _ *app, req vpnclient.ProbeRequest) (*vpnclient.Probe, error) {
		seen = req
		return answer(req)
	}
	t.Cleanup(func() { probeMachine = prev })
	return &seen
}

func reachedProbe(req vpnclient.ProbeRequest) (*vpnclient.Probe, error) {
	return &vpnclient.Probe{
		Machine: req.Machine, Endpoint: "203.0.113.9:4021", Result: vpnclient.ProbeReached,
		Admitted: true, Handshake: time.Now(), Elapsed: 180 * time.Millisecond,
		Detail: "a packet from here arrived on udp 4021 and was answered",
	}, nil
}

func TestServerProbeExitsZeroOnlyWhenAPacketCameBack(t *testing.T) {
	cases := []struct {
		result string
		want   int
		says   string
	}{
		{vpnclient.ProbeReached, ExitOK, "reached box at 203.0.113.9:4021"},
		{vpnclient.ProbeNoAnswer, ExitError, "no answer from box"},
		{vpnclient.ProbeUnproven, ExitError, "unproven: box"},
		{vpnclient.ProbeNoRoute, ExitError, "no route to 203.0.113.9:4021"},
	}
	for _, tc := range cases {
		t.Run(tc.result, func(t *testing.T) {
			isolateOnACommander(t)
			scriptedProbe(t, func(req vpnclient.ProbeRequest) (*vpnclient.Probe, error) {
				p, _ := reachedProbe(req)
				p.Result, p.Detail = tc.result, "what it saw"
				return p, nil
			})
			code, stdout, stderr := run(t, "hub", "probe", "box")
			if code != tc.want {
				t.Fatalf("exit %d, want %d\n%s", code, tc.want, stderr)
			}
			if code == 255 {
				t.Fatal("255 belongs to ssh, never to caramelo")
			}
			if !strings.Contains(stdout, tc.says) {
				t.Errorf("stdout = %q, want it to say %q", stdout, tc.says)
			}
		})
	}
}

func TestServerProbeCarriesTheMachineEndpointAndKey(t *testing.T) {
	isolateOnACommander(t)
	seen := scriptedProbe(t, reachedProbe)

	code, _, stderr := run(t, "hub", "probe", "box",
		"--endpoint", "203.0.113.9:4021", "--key", "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=",
		"--timeout", "3s")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if seen.Machine != "box" || seen.Endpoint != "203.0.113.9:4021" ||
		seen.Key != "Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=" || seen.Timeout != 3*time.Second {
		t.Errorf("the probe was asked for %+v", *seen)
	}
}

func TestServerProbeJSONCarriesTheWholeAnswer(t *testing.T) {
	isolateOnACommander(t)
	scriptedProbe(t, reachedProbe)

	code, stdout, stderr := run(t, "hub", "probe", "box", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var p vpnclient.Probe
	if err := json.Unmarshal([]byte(stdout), &p); err != nil {
		t.Fatalf("stdout is not a probe (%v):\n%s", err, stdout)
	}
	if p.Result != vpnclient.ProbeReached || p.Endpoint != "203.0.113.9:4021" || !p.Admitted {
		t.Errorf("probe = %+v", p)
	}
	if p.Handshake.IsZero() || p.Detail == "" {
		t.Errorf("the JSON dropped what the probe saw: %+v", p)
	}
}

func TestServerProbeFallsBackToTheDefaultMachine(t *testing.T) {
	isolateOnACommander(t)
	seen := scriptedProbe(t, reachedProbe)
	t.Setenv("CARAMELO_MACHINE", "other")

	if code, _, stderr := run(t, "hub", "probe"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if seen.Machine != "other" {
		t.Errorf("probed %q, want the machine this commander is pointed at", seen.Machine)
	}
}

func TestServerProbeWithNoMachineSaysHowToNameOne(t *testing.T) {
	isolateOnACommander(t)
	code, stdout, stderr := run(t, "hub", "probe")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d\n%s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--machine") {
		t.Errorf("stderr = %q, want it to say how to name a machine", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
}

func TestServerProbeReportsAnErrorRatherThanGuessing(t *testing.T) {
	isolateOnACommander(t)
	scriptedProbe(t, func(vpnclient.ProbeRequest) (*vpnclient.Probe, error) {
		return nil, errors.New("no public key for box")
	})
	code, _, stderr := run(t, "hub", "probe", "box")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "no public key") {
		t.Errorf("stderr = %q, want the reason it could not probe", stderr)
	}
}
