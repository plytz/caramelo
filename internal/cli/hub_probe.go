package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/vpnclient"
)

func init() {
	registerHub(func(a *app) *cobra.Command { return a.hubProbeCmd() })
}

var probeMachine = func(ctx context.Context, a *app, req vpnclient.ProbeRequest) (*vpnclient.Probe, error) {
	client, err := a.vpnClient()
	if err != nil {
		return nil, err
	}
	return client.Probe(ctx, req)
}

func (a *app) hubProbeCmd() *cobra.Command {
	var (
		endpoint string
		key      string
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "probe [machine|host[:port]]",
		Short: "Ask whether a machine's tunnel port answers from here",
		Long: `Sends a WireGuard handshake to the machine's UDP port from this computer
and reports what came back: reached, no-answer, unproven or no-route.

This is the only thing that can say a port is reachable. A machine reading its
own firewall can say it is not the one blocking a port; only a packet that
arrived from outside says the whole path is open.

Silence has two causes, and the answer names which one applies: a packet that
was dropped on the way, or a key this machine does not admit, which gets no
reply at all. A commander that has run 'caramelo vpn up' is admitted and its
silence means the packet was dropped; anything else is reported as unproven.`,
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			} else {
				m, err := a.vpnFleet()
				if err != nil {
					return err
				}
				target = m
			}
			p, err := probeMachine(cmd.Context(), a, vpnclient.ProbeRequest{
				Machine: target, Endpoint: endpoint, Key: key, Timeout: timeout,
			})
			if err != nil {
				return err
			}
			if err := a.printer().Result(p, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, probeLine(p))
				return err
			}); err != nil {
				return err
			}
			if !p.OK() {
				return &exitError{ExitError}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&endpoint, "endpoint", "", "the machine's UDP endpoint, host[:port] (default: the one this commander recorded)")
	f.StringVar(&key, "key", "", "the machine's WireGuard public key, for a computer that has never joined it")
	f.DurationVar(&timeout, "timeout", 0, "how long to wait for an answer (default: 10s)")
	return available(cmd, onCommander.or(onServer))
}

func probeLine(p *vpnclient.Probe) string {
	switch p.Result {
	case vpnclient.ProbeReached:
		return fmt.Sprintf("reached %s at %s in %s: %s", p.Machine, p.Endpoint, p.Elapsed, p.Detail)
	case vpnclient.ProbeNoAnswer:
		return fmt.Sprintf("no answer from %s at %s: %s", p.Machine, p.Endpoint, p.Detail)
	case vpnclient.ProbeNoRoute:
		return fmt.Sprintf("no route to %s: %s", p.Endpoint, p.Detail)
	}
	return fmt.Sprintf("unproven: %s at %s: %s", p.Machine, p.Endpoint, p.Detail)
}
