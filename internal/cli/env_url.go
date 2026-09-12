package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
)

func (e *envCmd) urlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "url NAME [SERVICE|DEP]",
		Short: "Print where an environment's services and dependencies answer",
		Long: `url prints the addresses of an environment:

  caramelo env url feat-x           # the first service's URL
  caramelo env url feat-x db        # one dependency's
  caramelo env url feat-x --json    # every service and dependency

Standard output carries the loopback address, which is the one that works on
the machine itself. Beside it, on standard error, is the environment's
.internal name and the port the application was written for: that is the
address from a computer that has joined the machine's network with
'caramelo vpn up' and installed transparent mode, and the one 'caramelo
connect' lends to a program that has not.`,

		Args: rangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			target := ""
			if len(args) == 2 {
				target = args[1]
			}
			urls, err := e.service().URLs(cmd.Context(), e.app, args[0], target)
			if err != nil {
				return err
			}
			if urls == nil {
				urls = []env.URL{}
			}
			public := e.publicURLs(cmd.Context(), args[0])
			return e.a.printer().Result(urls, func(w io.Writer) error {
				if err := writeURLs(w, urls, args[0], target); err != nil {
					return err
				}
				writeInternalURLs(e.a.stderr, urls, target)
				writePublicURLs(e.a.stderr, public, target)
				return nil
			})
		},
	}
	return cmd
}

func writeURLs(w io.Writer, urls []env.URL, name, target string) error {
	printed := printedURLs(urls, target)
	if target == "" && len(printed) == 0 {
		return fmt.Errorf("%s has no service answering anywhere yet: start it with `caramelo up %s`%s",
			name, name, otherNames(urls))
	}
	if len(printed) == 0 {
		return errors.New("no address")
	}
	for _, u := range printed {
		if _, err := fmt.Fprintln(w, u.URL); err != nil {
			return err
		}
	}
	return nil
}

func printedURLs(urls []env.URL, target string) []env.URL {
	if target != "" {
		return urls
	}
	for _, u := range urls {
		if u.Kind == env.KindService {
			return []env.URL{u}
		}
	}
	return nil
}

func (e *envCmd) publicURLs(ctx context.Context, name string) map[string]string {
	detail, err := e.service().Env(ctx, e.app, name)
	if err != nil || detail == nil {
		return nil
	}
	out := make(map[string]string, len(detail.Routes))
	for _, r := range detail.Routes {
		if _, taken := out[r.Service]; taken {
			continue
		}
		out[r.Service] = "https://" + r.Host
	}
	return out
}

func writePublicURLs(w io.Writer, public map[string]string, target string) {
	if w == nil || len(public) == 0 {
		return
	}
	if target != "" {
		if url, ok := public[target]; ok {
			fmt.Fprintf(w, "public: %s\n", url)
		}
		return
	}

	for _, service := range sortedKeys(public) {
		fmt.Fprintf(w, "public: %s (%s)\n", public[service], service)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeInternalURLs(w io.Writer, urls []env.URL, target string) {
	if w == nil {
		return
	}
	for _, u := range printedURLs(urls, target) {
		if u.InternalURL == "" {
			continue
		}
		fmt.Fprintf(w, "on the machine's network: %s (%s)\n", u.InternalURL, u.InternalAddress)
	}
}

func otherNames(urls []env.URL) string {
	names := make([]string, 0, len(urls))
	for _, u := range urls {
		names = append(names, u.Name)
	}
	if len(names) == 0 {
		return ""
	}
	return ", or ask for one of: " + strings.Join(names, ", ")
}
