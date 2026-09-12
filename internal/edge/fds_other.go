//go:build !unix

package edge

import "errors"

const (
	sockStream   = 1
	sockDatagram = 2
)

func closeOnExec(int) {}

func sockInfo(int) (kind, port int, err error) {
	return 0, 0, errors.New("socket activation needs a unix system")
}
