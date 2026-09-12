package ports

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/plytz/caramelo/internal/state"
)

const (
	Base = 20000

	Ceiling = 32000

	BlockSize = 32

	LegacyBlockSize = 16
)

const BlockCount = (Ceiling - Base) / BlockSize

type Allocator interface {
	Allocate(ctx context.Context, envID int64) (base int, err error)
	Release(ctx context.Context, envID int64) error
}

type Binder interface {
	CanBind(port int) bool
}

type LocalBinder struct{}

func (LocalBinder) CanBind(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func Block(base int) []int {
	out := make([]int, 0, BlockSize)
	for i := 0; i < BlockSize; i++ {
		out = append(out, base+i)
	}
	return out
}

func ValidBase(base int) bool {
	return base >= Base && base < Ceiling && (base-Base)%BlockSize == 0
}

type Span struct {
	Base  int
	Count int
}

func (s Span) End() int { return s.Base + s.size() }

func (s Span) size() int {
	if s.Count <= 0 {
		return BlockSize
	}
	return s.Count
}

func (s Span) Overlaps(other Span) bool {
	return s.Base < other.End() && other.Base < s.End()
}

var ErrExhausted = fmt.Errorf("no free port block between %d and %d", Base, Ceiling)

type BlockAllocator struct {
	Store  state.Store
	Binder Binder

	Log io.Writer
}

func New(store state.Store, b Binder) *BlockAllocator {
	if b == nil {
		b = LocalBinder{}
	}
	return &BlockAllocator{Store: store, Binder: b}
}

var _ Allocator = (*BlockAllocator)(nil)

func (a *BlockAllocator) Allocate(ctx context.Context, envID int64) (int, error) {
	taken, err := a.taken(ctx, envID)
	if err != nil {
		return 0, err
	}
	binder := a.binder()
	for base := Base; base < Ceiling; base += BlockSize {
		candidate := Span{Base: base, Count: BlockSize}
		if overlapping, busy := overlap(taken, candidate); busy {
			a.logf("port block %d-%d skipped: %d-%d is allocated to another environment",
				base, base+BlockSize-1, overlapping.Base, overlapping.End()-1)
			continue
		}
		if busy, ok := probe(binder, base); !ok {
			a.logf("port block %d-%d skipped: %d is already in use", base, base+BlockSize-1, busy)
			continue
		}
		return base, nil
	}
	return 0, ErrExhausted
}

func (a *BlockAllocator) Release(ctx context.Context, envID int64) error { return nil }

func (a *BlockAllocator) taken(ctx context.Context, envID int64) ([]Span, error) {
	if a.Store == nil {
		return nil, fmt.Errorf("ports: no store")
	}
	envs, err := a.Store.Envs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("ports: read allocated blocks: %w", err)
	}
	out := make([]Span, 0, len(envs))
	for _, e := range envs {
		if envID != 0 && e.ID == envID {
			continue
		}
		if e.PortBase == 0 {
			continue
		}
		out = append(out, Span{Base: e.PortBase, Count: e.PortCount})
	}
	return out, nil
}

func overlap(taken []Span, candidate Span) (Span, bool) {
	for _, s := range taken {
		if s.Overlaps(candidate) {
			return s, true
		}
	}
	return Span{}, false
}

func (a *BlockAllocator) binder() Binder {
	if a.Binder == nil {
		return LocalBinder{}
	}
	return a.Binder
}

func (a *BlockAllocator) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	fmt.Fprintf(a.Log, format+"\n", args...)
}

func probe(b Binder, base int) (busy int, ok bool) {
	for _, p := range Block(base) {
		if !b.CanBind(p) {
			return p, false
		}
	}
	return 0, true
}
