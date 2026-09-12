package setup

func HostSteps() (before, after []Step) {
	before = []Step{
		NewPreflightStep(),
		NewGaugeStep(),
		NewUserStep(),
		NewDirsStep(),
		NewHostConfigStep(),
	}
	after = []Step{
		NewVPNStep(),

		NewVaultStep(),
		NewCaramelodStep(),
		NewEdgeStep(),
		NewPeerStep(),

		NewJoinStep(),
		NewSummaryStep(),
	}
	return before, after
}
