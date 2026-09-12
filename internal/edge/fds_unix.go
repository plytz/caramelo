//go:build unix

package edge

import (
	"fmt"
	"syscall"
)

const (
	sockStream   = syscall.SOCK_STREAM
	sockDatagram = syscall.SOCK_DGRAM
)

func closeOnExec(fd int) { syscall.CloseOnExec(fd) }

func sockInfo(fd int) (kind, port int, err error) {
	kind, err = syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		return 0, 0, fmt.Errorf("getsockopt(SO_TYPE): %w", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return 0, 0, fmt.Errorf("getsockname: %w", err)
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		port = a.Port
	case *syscall.SockaddrInet6:
		port = a.Port
	default:
		return 0, 0, fmt.Errorf("socket is not IP (%T)", sa)
	}
	return kind, port, nil
}
