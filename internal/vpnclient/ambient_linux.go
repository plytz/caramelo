package vpnclient

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func raiseNetAdmin() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("read this process's capabilities: %w", err)
	}
	i, mask := capIndex(unix.CAP_NET_ADMIN)
	if data[i].Permitted&mask == 0 {
		return fmt.Errorf("this binary does not hold CAP_NET_ADMIN " +
			"(run 'sudo caramelo vpn install' to grant it)")
	}
	data[i].Inheritable |= mask
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("make CAP_NET_ADMIN inheritable: %w", err)
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, unix.CAP_NET_ADMIN, 0, 0); err != nil {
		return fmt.Errorf("raise CAP_NET_ADMIN into the ambient set: %w", err)
	}
	return nil
}

func capIndex(cap uintptr) (int, uint32) {
	return int(cap / 32), uint32(1) << (cap % 32)
}
