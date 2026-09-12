//go:build integration

package itest

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	StateClean       = "clean"
	StateProvisioned = "provisioned"
)

const TargetDocker = "docker"

const (
	RoleHub    = "hub"
	RoleNode   = "node"
	RoleClient = "client"
)

const (
	BinEnv     = "CARAMELO_ITEST_BIN"
	HostBinEnv = "CARAMELO_ITEST_HOST_BIN"
	KeepEnv    = "CARAMELO_ITEST_KEEP"
	FactorEnv  = "CARAMELO_ITEST_FACTOR"
)

const (
	LabelKey   = "caramelo.itest"
	LabelValue = "1"
	SuiteLabel = "caramelo.itest.suite"
)

func label() string { return LabelKey + "=" + LabelValue }

const (
	LoginUser = "debian"
	LoginHome = "/home/" + LoginUser
)

const (
	CarameloUser    = "caramelo"
	CarameloBinary  = "/usr/local/bin/caramelo"
	CarameloSocket  = "/run/caramelo/caramelod.sock"
	CarameloSSHPort = 4022
	RemoteBin       = "/var/tmp/caramelo"
	DockerDataDir   = "/mnt/caramelo/docker"
)

const (
	ACMEDirectory = "https://127.0.0.1:14000/dir"
	ACMEEmail     = "lab@example.test"
)

const (
	PebbleDirectory = ACMEDirectory
	LabACMEEmail    = ACMEEmail
)

type Port struct {
	Number int
	Proto  string
}

func (p Port) String() string { return strconv.Itoa(p.Number) + "/" + p.Proto }

func TCP(n int) Port { return Port{Number: n, Proto: "tcp"} }

func UDP(n int) Port { return Port{Number: n, Proto: "udp"} }

var DefaultPorts = []Port{
	TCP(22),
	TCP(CarameloSSHPort),
	UDP(4021),
	TCP(80),
	TCP(443),
	UDP(443),
}

func GoArch(unameM string) string {
	switch strings.TrimSpace(unameM) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	}
	return strings.TrimSpace(unameM)
}

type Budgets struct {
	Factor     float64
	Boot       time.Duration
	Setup      time.Duration
	Reset      time.Duration
	Suite      time.Duration
	PerMachine time.Duration
}

const DefaultPerMachine = 5 * time.Minute

func (b Budgets) For(d time.Duration) time.Duration {
	if b.Factor <= 1 {
		return d
	}
	scaled := time.Duration(float64(d) * b.Factor)
	if scaled < time.Second {
		return scaled.Round(time.Millisecond)
	}
	return scaled.Round(time.Second)
}

func (b Budgets) SuiteFor(machines int) time.Duration {
	if machines < 1 {
		machines = 1
	}
	per := b.PerMachine
	if per <= 0 {
		per = DefaultPerMachine
	}
	return b.Suite + time.Duration(machines-1)*b.For(per)
}

var sshBudgets func() Budgets

func Budget() Budgets {
	if sshBudgets != nil && strings.TrimSpace(os.Getenv(InventoryEnv)) != "" {
		return sshBudgets()
	}
	factor := 1.0
	if v := strings.TrimSpace(os.Getenv(FactorEnv)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			factor = f
		}
	}
	b := Budgets{
		Factor:     factor,
		Boot:       2 * time.Minute,
		Setup:      20 * time.Minute,
		Reset:      5 * time.Minute,
		Suite:      40 * time.Minute,
		PerMachine: DefaultPerMachine,
	}
	b.Boot = b.For(b.Boot)
	b.Setup = b.For(b.Setup)
	b.Reset = b.For(b.Reset)
	b.Suite = b.For(b.Suite)
	return b
}

func Scale(d time.Duration) time.Duration { return Budget().For(d) }

func Keep() bool { return strings.TrimSpace(os.Getenv(KeepEnv)) == "1" }
