//go:build !unix

package task

import "fmt"

func ownerOf(path string) (int, int, error) {
	return 0, 0, fmt.Errorf("stat %s: this machine does not report a file's owner", path)
}
