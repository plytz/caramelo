package machine

import (
	"context"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

type SwapEntry struct {
	Name      string
	SizeBytes int64
}

func ParseSwapon(out string) []SwapEntry {
	var entries []SwapEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		e := SwapEntry{Name: fields[0]}
		if len(fields) > 1 {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				e.SizeBytes = n
			}
		}
		entries = append(entries, e)
	}
	return entries
}

func SwaponCmd() runner.Cmd {
	return runner.Cmd{Name: "swapon", Args: []string{"--show=NAME,SIZE", "--bytes", "--noheadings"}}
}

func ReadSwap(ctx context.Context, run runner.Runner, swapfile string) (totalBytes int64, managed bool) {
	if run == nil {
		return 0, false
	}
	res, err := run.Run(ctx, SwaponCmd())
	if err != nil || res.ExitCode != 0 {
		return 0, false
	}
	for _, e := range ParseSwapon(res.Stdout) {
		totalBytes += e.SizeBytes
		if swapfile != "" && e.Name == swapfile {
			managed = true
		}
	}
	return totalBytes, managed
}
