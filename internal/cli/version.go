package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"
)

type versionInfo struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func currentVersion() versionInfo {
	return versionInfo{
		Version: version,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

func (a *app) versionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the caramelo version",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			v := currentVersion()
			return a.printer().Result(v, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "caramelo %s (%s %s/%s)\n", v.Version, v.Go, v.OS, v.Arch)
				return err
			})
		},
	}
	return available(cmd, always)
}
