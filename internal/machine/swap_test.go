package machine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/plytz/caramelo/internal/runner"
)

type swaponRunner struct {
	out  string
	code int
	err  error
	ran  []string
}

func (r *swaponRunner) Run(ctx context.Context, c runner.Cmd) (runner.Result, error) {
	r.ran = append(r.ran, c.Name)
	if r.err != nil {
		return runner.Result{}, r.err
	}
	return runner.Result{Stdout: r.out, ExitCode: r.code}, nil
}

func TestParseSwapon(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []SwapEntry
	}{
		{name: "nothing", out: ""},
		{name: "only blank lines", out: "\n  \n"},
		{
			name: "one swapfile",
			out:  "/var/lib/caramelo.swapfile 4294967296\n",
			want: []SwapEntry{{Name: "/var/lib/caramelo.swapfile", SizeBytes: 4294967296}},
		},
		{
			name: "a partition and a file",
			out:  "/dev/sda2 2147483648\n/var/lib/caramelo.swapfile 4294967296\n",
			want: []SwapEntry{
				{Name: "/dev/sda2", SizeBytes: 2147483648},
				{Name: "/var/lib/caramelo.swapfile", SizeBytes: 4294967296},
			},
		},
		{
			name: "a size that is not a number is still an entry",
			out:  "/dev/sda2 4G\n",
			want: []SwapEntry{{Name: "/dev/sda2"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseSwapon(tc.out)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseSwapon(%q) = %+v, want %+v", tc.out, got, tc.want)
			}
		})
	}
}

func TestReadSwap(t *testing.T) {
	tests := []struct {
		name        string
		run         *swaponRunner
		swapfile    string
		wantTotal   int64
		wantManaged bool
	}{
		{
			name:        "the swapfile caramelo made",
			run:         &swaponRunner{out: "/var/lib/caramelo.swapfile 4294967296\n"},
			swapfile:    "/var/lib/caramelo.swapfile",
			wantTotal:   4294967296,
			wantManaged: true,
		},
		{
			name:      "somebody else's swap partition",
			run:       &swaponRunner{out: "/dev/sda2 2147483648\n"},
			swapfile:  "/var/lib/caramelo.swapfile",
			wantTotal: 2147483648,
		},
		{
			name:        "both, counted together",
			run:         &swaponRunner{out: "/dev/sda2 2147483648\n/var/lib/caramelo.swapfile 4294967296\n"},
			swapfile:    "/var/lib/caramelo.swapfile",
			wantTotal:   6442450944,
			wantManaged: true,
		},
		{name: "no swap at all", run: &swaponRunner{}, swapfile: "/var/lib/caramelo.swapfile"},
		{name: "swapon is not there", run: &swaponRunner{code: 127}, swapfile: "/var/lib/caramelo.swapfile"},
		{name: "the runner itself fails", run: &swaponRunner{err: errors.New("no ssh")}, swapfile: "/var/lib/caramelo.swapfile"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total, managed := ReadSwap(context.Background(), tc.run, tc.swapfile)
			if total != tc.wantTotal || managed != tc.wantManaged {
				t.Errorf("ReadSwap = %d, %v, want %d, %v", total, managed, tc.wantTotal, tc.wantManaged)
			}
		})
	}
}

func TestReadSwapWithoutARunner(t *testing.T) {
	total, managed := ReadSwap(context.Background(), nil, "/var/lib/caramelo.swapfile")
	if total != 0 || managed {
		t.Errorf("ReadSwap(nil) = %d, %v, want 0, false", total, managed)
	}
}
