package remote

import (
	"fmt"
	"os"
)

func SSHClient() (bin string, extra []string, err error) {
	bin = os.Getenv("CARAMELO_SSH")
	if bin == "" {
		bin = "ssh"
	}
	if opts := os.Getenv("CARAMELO_SSH_OPTS"); opts != "" {
		extra, err = SplitWords(opts)
		if err != nil {
			return "", nil, fmt.Errorf("CARAMELO_SSH_OPTS: %w", err)
		}
	}
	return bin, extra, nil
}
