package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func fakeGauge(t *testing.T, rec *machine.Record, err error) {
	t.Helper()
	prev := gauge
	gauge = func(context.Context, runner.Runner, serverconfig.Config, string, string) (*machine.Record, error) {
		return rec, err
	}
	t.Cleanup(func() { gauge = prev })
}

func bigEnoughRecord() *machine.Record {
	return &machine.Record{
		Hostname: "box",
		OS:       machine.OS{ID: "debian", VersionID: "13", Arch: "x86_64", Virt: "kvm"},
		CPU:      machine.CPU{Count: 2},
		Memory:   machine.Memory{TotalBytes: 4 << 30},
		DataDir:  machine.Mount{Path: "/mnt/caramelo", AvailBytes: 90 << 30},
	}
}

func TestGaugeDescribesTheMachine(t *testing.T) {
	rec := bigEnoughRecord()
	fakeGauge(t, rec, nil)
	step := NewGaugeStep()
	env, log := testEnv(t, testutil.New())

	done, detail, err := step.Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	for _, want := range []string{"2 vCPU", "4.0 GiB RAM", "90.0 GiB free on /mnt/caramelo", "debian 13", "kvm"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}
	if !strings.Contains(log.String(), "machine: ") {
		t.Errorf("log %q does not summarise the machine", log.String())
	}
	if step.Record != rec {
		t.Errorf("the record was not kept for the caller")
	}
}

func TestGaugeRefusesATinyMachine(t *testing.T) {
	rec := bigEnoughRecord()
	rec.Memory.TotalBytes = 512 << 20
	rec.DataDir.AvailBytes = 2 << 30
	fakeGauge(t, rec, nil)
	env, _ := testEnv(t, testutil.New())

	_, _, err := NewGaugeStep().Check(context.Background(), env)
	if err == nil {
		t.Fatal("Check() accepted a machine too small to run anything")
	}
	for _, want := range []string{"512.0 MiB of RAM", "2.0 GiB free", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

func TestGaugeForceContinuesAndWarns(t *testing.T) {
	rec := bigEnoughRecord()
	rec.Memory.TotalBytes = 512 << 20
	fakeGauge(t, rec, nil)
	env, log := testEnv(t, testutil.New())
	env.Opts.Force = true

	done, _, err := NewGaugeStep().Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; want --force to continue", done, err)
	}
	if !strings.Contains(log.String(), "warning: machine too small") {
		t.Errorf("log %q does not warn", log.String())
	}
}

func TestGaugeAsksAnAncestorForFreeSpace(t *testing.T) {
	rec := bigEnoughRecord()
	rec.DataDir.AvailBytes = 0
	fakeGauge(t, rec, nil)

	run := testutil.New()
	run.Exit("df -B1 --output=avail /mnt/caramelo", 1)
	run.Stdout("df -B1 --output=avail /mnt", "      Avail\n98999504896\n")
	env, _ := testEnv(t, run)

	done, detail, err := NewGaugeStep().Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %v; want done", done, err)
	}
	if !strings.Contains(detail, "92.2 GiB free") {
		t.Errorf("detail %q does not use the parent filesystem's free space", detail)
	}
}

func TestGaugeFailsWhenTheMachineCannotBeRead(t *testing.T) {
	fakeGauge(t, nil, errors.New("no /proc/meminfo"))
	env, _ := testEnv(t, testutil.New())
	if _, _, err := NewGaugeStep().Check(context.Background(), env); err == nil {
		t.Fatal("Check() ignored a failing gauge")
	}
}

func TestParseDFAvail(t *testing.T) {
	got, err := parseDFAvail("      Avail\n98999504896\n")
	if err != nil || got != 98999504896 {
		t.Fatalf("parseDFAvail() = %d, %v", got, err)
	}
	if _, err := parseDFAvail("Avail\n"); err == nil {
		t.Error("parseDFAvail() accepted a header with no value")
	}
}

func TestBytesIEC(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{0, "0 B"}, {512, "512 B"}, {1 << 20, "1.0 MiB"}, {1541255168, "1.4 GiB"}} {
		if got := bytesIEC(tc.in); got != tc.want {
			t.Errorf("bytesIEC(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGaugeAcceptsAOneGigabyteBoard(t *testing.T) {
	rec := bigEnoughRecord()
	rec.Memory.TotalBytes = 926820 * 1024
	fakeGauge(t, rec, nil)
	env, _ := testEnv(t, testutil.New())

	done, detail, err := NewGaugeStep().Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want a 1 GB board accepted", done, detail, err)
	}
	if !strings.Contains(detail, "905.1 MiB RAM") {
		t.Errorf("detail %q does not report the memory it saw", detail)
	}
}
