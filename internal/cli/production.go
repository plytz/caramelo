package cli

import (
	"time"

	"github.com/spf13/cobra"
)

type deployFlags struct {
	ref      string
	noWatch  bool
	timeout  time.Duration
	force    bool
	services []string

	to string

	noPush bool
}

func init() {
	register(func(a *app) *cobra.Command { return a.buildCmd() })
	register(func(a *app) *cobra.Command { return a.deployCmd() })
	register(func(a *app) *cobra.Command { return a.promoteCmd() })
	register(func(a *app) *cobra.Command { return a.rollbackCmd() })
	register(func(a *app) *cobra.Command { return a.releasesCmd() })
	register(func(a *app) *cobra.Command { return a.eventsCmd() })
	register(func(a *app) *cobra.Command { return a.secretsCmd() })
}

func (t *envTargets) pushBeforeRelease(cmd *cobra.Command, f *deployFlags) error {

	return t.pushBeforeUp(cmd, f.noPush, false)
}
