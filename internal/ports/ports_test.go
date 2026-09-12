package ports

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/state"
)

type fakeStore struct {
	state.Store
	envs []state.EnvRecord
	err  error
}

func (f *fakeStore) Envs(ctx context.Context, app string) ([]state.EnvRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.envs, nil
}

type fakeBinder struct{ busy map[int]bool }

func (b fakeBinder) CanBind(port int) bool { return !b.busy[port] }

func TestBlockGeometry(t *testing.T) {
	if got := len(Block(Base)); got != BlockSize {
		t.Fatalf("Block length = %d, want %d", got, BlockSize)
	}
	if got, want := Block(Base)[BlockSize-1], Base+BlockSize-1; got != want {
		t.Errorf("last port = %d, want %d", got, want)
	}
	for _, base := range []int{Base, Base + BlockSize, Ceiling - BlockSize} {
		if !ValidBase(base) {
			t.Errorf("ValidBase(%d) = false, want true", base)
		}
	}
	for _, base := range []int{Base - BlockSize, Base + 1, Ceiling, Ceiling + BlockSize} {
		if ValidBase(base) {
			t.Errorf("ValidBase(%d) = true, want false", base)
		}
	}
	if BlockCount != 375 {
		t.Errorf("BlockCount = %d, want 375", BlockCount)
	}
	if BlockSize != 2*LegacyBlockSize {
		t.Errorf("BlockSize = %d, want twice the legacy %d", BlockSize, LegacyBlockSize)
	}
}

func TestAllocateSkipsBlocksOverlappingLegacySpans(t *testing.T) {
	store := &fakeStore{envs: []state.EnvRecord{

		{ID: 1, PortBase: Base, PortCount: LegacyBlockSize},
		{ID: 2, PortBase: Base + LegacyBlockSize, PortCount: LegacyBlockSize},

		{ID: 3, PortBase: Base + BlockSize + LegacyBlockSize, PortCount: LegacyBlockSize},
	}}
	var log strings.Builder
	a := &BlockAllocator{Store: store, Binder: fakeBinder{}, Log: &log}

	got, err := a.Allocate(context.Background(), 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if want := Base + 2*BlockSize; got != want {
		t.Fatalf("Allocate = %d, want %d (the first block no legacy env overlaps)", got, want)
	}
	if !strings.Contains(log.String(), "allocated to another environment") {
		t.Errorf("the overlapping blocks were not reported: %q", log.String())
	}
}

func TestAllocateMixedSizes(t *testing.T) {
	store := &fakeStore{envs: []state.EnvRecord{
		{ID: 1, PortBase: Base, PortCount: BlockSize},
		{ID: 2, PortBase: Base + BlockSize, PortCount: LegacyBlockSize},
	}}
	a := &BlockAllocator{Store: store, Binder: fakeBinder{}}

	got, err := a.Allocate(context.Background(), 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if want := Base + 2*BlockSize; got != want {
		t.Fatalf("Allocate = %d, want %d", got, want)
	}
}

func TestSpanOverlaps(t *testing.T) {
	block := Span{Base: Base, Count: BlockSize}
	for _, tc := range []struct {
		name  string
		other Span
		want  bool
	}{
		{"the same block", Span{Base, BlockSize}, true},
		{"a legacy block inside it", Span{Base + LegacyBlockSize, LegacyBlockSize}, true},
		{"a legacy block ending in it", Span{Base - LegacyBlockSize, 2 * LegacyBlockSize}, true},
		{"the next block", Span{Base + BlockSize, BlockSize}, false},
		{"the block before", Span{Base - BlockSize, BlockSize}, false},
		{"a row with no count", Span{Base: Base + BlockSize}, false},
	} {
		if got := block.Overlaps(tc.other); got != tc.want {
			t.Errorf("%s: Overlaps(%v) = %v, want %v", tc.name, tc.other, got, tc.want)
		}
		if got := tc.other.Overlaps(block); got != tc.want {
			t.Errorf("%s: reversed Overlaps = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAllocateSkipsTakenAndUnbindableBlocks(t *testing.T) {
	store := &fakeStore{envs: []state.EnvRecord{
		{ID: 1, PortBase: Base},
		{ID: 2, PortBase: Base + BlockSize},
	}}

	binder := fakeBinder{busy: map[int]bool{Base + 2*BlockSize + 3: true}}
	var log strings.Builder
	a := &BlockAllocator{Store: store, Binder: binder, Log: &log}

	got, err := a.Allocate(context.Background(), 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if want := Base + 3*BlockSize; got != want {
		t.Fatalf("Allocate = %d, want %d", got, want)
	}
	if !strings.Contains(log.String(), "already in use") {
		t.Errorf("skipped block was not reported: %q", log.String())
	}
}

func TestAllocateIgnoresOwnBlock(t *testing.T) {
	store := &fakeStore{envs: []state.EnvRecord{{ID: 7, PortBase: Base}}}
	a := &BlockAllocator{Store: store, Binder: fakeBinder{}}

	if got, err := a.Allocate(context.Background(), 7); err != nil || got != Base {
		t.Fatalf("Allocate(own env) = %d, %v; want %d, nil", got, err, Base)
	}
	if got, err := a.Allocate(context.Background(), 0); err != nil || got != Base+BlockSize {
		t.Fatalf("Allocate(new env) = %d, %v; want %d, nil", got, err, Base+BlockSize)
	}
}

func TestAllocateExhausted(t *testing.T) {
	store := &fakeStore{}
	for base := Base; base < Ceiling; base += BlockSize {
		store.envs = append(store.envs, state.EnvRecord{ID: int64(base), PortBase: base})
	}
	a := &BlockAllocator{Store: store, Binder: fakeBinder{}}
	if _, err := a.Allocate(context.Background(), 0); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Allocate = %v, want ErrExhausted", err)
	}
}

func TestAllocateStoreError(t *testing.T) {
	a := &BlockAllocator{Store: &fakeStore{err: errors.New("boom")}, Binder: fakeBinder{}}
	_, err := a.Allocate(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Allocate = %v, want the store error wrapped", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	a := New(&fakeStore{}, fakeBinder{})
	for i := 0; i < 2; i++ {
		if err := a.Release(context.Background(), 1); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
}

func TestLocalBinder(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind on this host: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port

	var b LocalBinder
	if b.CanBind(port) {
		t.Errorf("CanBind(%d) = true while we hold it", port)
	}
	l.Close()
	if !b.CanBind(port) {
		t.Errorf("CanBind(%d) = false after releasing it", port)
	}
}
