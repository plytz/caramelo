//go:build unix

package task

import (
	"fmt"
	"os"
	"syscall"
)

func ownerOf(path string) (int, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: this machine does not report a file's owner", path)
	}
	return int(st.Uid), int(st.Gid), nil
}
