package fleet

import "github.com/plytz/caramelo/internal/machine"

func gauge(available, reserved int64) *machine.Record {
	return &machine.Record{
		Memory:   machine.Memory{TotalBytes: available, AvailableBytes: available},
		Reserved: machine.Reserved{MemoryBytes: reserved},
	}
}
