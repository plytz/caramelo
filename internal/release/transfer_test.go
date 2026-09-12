package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/runtime"
)

type fakeMover struct {
	mu sync.Mutex

	arch   string
	images map[string]runtime.ImageInfo

	loads   [][]string
	removed []string
	saveErr error
	loadErr error

	loadsAs []string
}

func newMover(arch string, refs ...string) *fakeMover {
	m := &fakeMover{arch: arch, images: map[string]runtime.ImageInfo{}}
	for _, ref := range refs {
		m.hold(ref, arch)
	}
	return m
}

func (m *fakeMover) hold(ref, arch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.images[ref] = runtime.ImageInfo{Ref: ref, ID: "sha256:" + ref, Arch: arch, Size: 1024}
}

func (m *fakeMover) ImageInfo(_ context.Context, ref string) (runtime.ImageInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.images[ref]
	if !ok {
		return runtime.ImageInfo{}, fmt.Errorf("image %s: %w", ref, runtime.ErrNotFound)
	}
	return info, nil
}

func (m *fakeMover) SaveImages(_ context.Context, refs []string, out io.Writer) (int64, error) {
	if m.saveErr != nil {
		return 0, m.saveErr
	}
	body := "tar:" + strings.Join(refs, ",")
	n, err := io.WriteString(out, body)
	return int64(n), err
}

func (m *fakeMover) LoadImages(_ context.Context, in io.Reader) ([]string, error) {
	body, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	refs := m.loadsAs
	if refs == nil {
		refs = strings.Split(strings.TrimPrefix(string(body), "tar:"), ",")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := m.images[ref]; !ok {
			m.images[ref] = runtime.ImageInfo{Ref: ref, ID: "sha256:" + ref, Arch: m.loadArch(), Size: 1024}
		}
		loaded = append(loaded, ref)
	}
	sort.Strings(loaded)
	m.loads = append(m.loads, loaded)
	return loaded, nil
}

func (m *fakeMover) loadArch() string { return m.arch }

func (m *fakeMover) RemoveImage(_ context.Context, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.images, ref)
	m.removed = append(m.removed, ref)
	return nil
}

func (m *fakeMover) has(ref string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.images[ref]
	return ok
}

type fakeImages struct {
	mu   sync.Mutex
	rows map[string]Image
	err  error
}

func newImages() *fakeImages { return &fakeImages{rows: map[string]Image{}} }

func (f *fakeImages) PutImage(_ context.Context, im Image) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[fmt.Sprintf("%d/%s/%s/%s", im.ReleaseID, im.Service, im.Arch, im.Machine)] = im
	return nil
}

func (f *fakeImages) Images(_ context.Context, id int64) (Images, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := Images{}
	for _, im := range f.rows {
		if im.ReleaseID == id {
			out = append(out, im)
		}
	}
	out.Sort()
	return out, nil
}

func (f *fakeImages) RemoveImages(_ context.Context, id int64, machine string) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, im := range f.rows {
		if im.ReleaseID == id && (machine == "" || im.Machine == machine) {
			delete(f.rows, k)
		}
	}
	return nil
}

var frozen = time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC)

func refsOf(r *Release) []string { return r.ImageList() }

func TestTransferSendsWhatItWasAskedForAndCountsIt(t *testing.T) {
	r := oneRelease()
	sender := newMover("arm64", refsOf(r)...)
	tr := &Transfer{Mover: sender, Machine: "m2", Arch: "arm64", Now: func() time.Time { return frozen }}

	var wire bytes.Buffer
	res, err := tr.Send(context.Background(), TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, &wire)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Bytes != int64(wire.Len()) || res.Bytes == 0 {
		t.Errorf("counted %d bytes, wrote %d", res.Bytes, wire.Len())
	}
	if len(res.Images) != 2 {
		t.Fatalf("reported %d images, want 2", len(res.Images))
	}
	for _, im := range res.Images {
		if im.Machine != "m2" || im.Arch != "arm64" || im.ReleaseID != 7 || im.ID == "" {
			t.Errorf("row = %+v, want m2/arm64/7 with a content address", im)
		}
	}
	if res.Images[0].Service != "web" || res.Images[1].Service != "worker" {
		t.Errorf("services = %q, %q, want web then worker", res.Images[0].Service, res.Images[1].Service)
	}
}

func TestTransferWillNotSendWhatItDoesNotHold(t *testing.T) {
	r := oneRelease()
	sender := newMover("arm64", r.Images["web"])
	tr := &Transfer{Mover: sender, Machine: "m2", Arch: "arm64"}

	var wire bytes.Buffer
	_, err := tr.Send(context.Background(), TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, &wire)
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Send of an image it lacks: %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "m2") || !strings.Contains(err.Error(), "worker") {
		t.Errorf("error %q does not name the machine and the image", err)
	}

	if wire.Len() != 0 {
		t.Errorf("wrote %d bytes before refusing", wire.Len())
	}
}

func TestTransferRefusesTheOtherArchitectureAtBothEnds(t *testing.T) {
	r := oneRelease()
	sender := newMover("arm64", refsOf(r)...)
	tr := &Transfer{Mover: sender, Machine: "m2", Arch: "arm64"}

	_, err := tr.Send(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "amd64"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "amd64") || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("Send of amd64 images from an arm64 machine: %v, want both architectures named", err)
	}
	_, err = tr.Receive(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "amd64"}, strings.NewReader("tar:"))
	if err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("Receive of amd64 images on an arm64 machine: %v, want a refusal", err)
	}
}

func TestTransferReceivesAndRecordsWhereTheImagesNowAre(t *testing.T) {
	r := oneRelease()
	sender := newMover("arm64", refsOf(r)...)
	receiver := newMover("arm64")
	rows := newImages()

	from := &Transfer{Mover: sender, Machine: "m2", Arch: "arm64", Now: func() time.Time { return frozen }}
	to := &Transfer{Mover: receiver, Store: rows, Machine: "m1", Arch: "arm64",
		Now: func() time.Time { return frozen }}

	var wire bytes.Buffer
	req := TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}
	if _, err := from.Send(context.Background(), req, &wire); err != nil {
		t.Fatalf("Send: %v", err)
	}
	res, err := to.Receive(context.Background(), req, &wire)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if res.Bytes == 0 {
		t.Error("counted no bytes on the receiving end")
	}
	for _, ref := range refsOf(r) {
		if !receiver.has(ref) {
			t.Errorf("%s did not arrive", ref)
		}
	}
	stored, err := rows.Images(context.Background(), 7)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("recorded %d rows, want 2", len(stored))
	}
	for _, im := range stored {
		if im.Machine != "m1" || im.Arch != "arm64" || im.BuiltAt != frozen {
			t.Errorf("row = %+v, want it recorded against m1/arm64 at the moment it arrived", im)
		}
	}

	if got := stored.Machines("arm64", r.Services()); strings.Join(got, ",") != "m1" {
		t.Errorf("Machines = %v, want m1", got)
	}
}

func TestTransferReceivedTwiceChangesNothing(t *testing.T) {
	r := oneRelease()
	receiver := newMover("arm64")
	rows := newImages()
	to := &Transfer{Mover: receiver, Store: rows, Machine: "m1", Arch: "arm64",
		Now: func() time.Time { return frozen }}
	req := TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}

	for i := 0; i < 2; i++ {
		if _, err := to.Receive(context.Background(), req, strings.NewReader("tar:"+strings.Join(refsOf(r), ","))); err != nil {
			t.Fatalf("Receive %d: %v", i+1, err)
		}
	}
	stored, err := rows.Images(context.Background(), 7)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(stored) != 2 {
		t.Errorf("recorded %d rows after two transfers, want 2", len(stored))
	}
}

func TestTransferUndoesAnArchiveThatBroughtTheWrongThing(t *testing.T) {
	r := oneRelease()
	receiver := newMover("arm64")

	receiver.loadsAs = []string{r.Images["web"]}
	rows := newImages()
	to := &Transfer{Mover: receiver, Store: rows, Machine: "m1", Arch: "arm64"}

	_, err := to.Receive(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, strings.NewReader("whatever"))
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("Receive of a short archive: %v, want the missing image named", err)
	}
	if receiver.has(r.Images["web"]) {
		t.Error("the half that arrived was kept: a machine must not be left holding half a release")
	}
	if stored, _ := rows.Images(context.Background(), 7); len(stored) != 0 {
		t.Errorf("recorded %d rows for a transfer that failed, want none", len(stored))
	}
}

func TestTransferUndoesAnImageOfTheWrongArchitecture(t *testing.T) {

	r := oneRelease()
	receiver := newMover("arm64")
	receiver.arch = "amd64"
	rows := newImages()
	to := &Transfer{Mover: receiver, Store: rows, Machine: "m1", Arch: "arm64"}

	_, err := to.Receive(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"},
		strings.NewReader("tar:"+strings.Join(refsOf(r), ",")))
	if err == nil || !strings.Contains(err.Error(), "amd64") || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("Receive of amd64 bytes under an arm64 tag: %v, want both named", err)
	}
	for _, ref := range refsOf(r) {
		if receiver.has(ref) {
			t.Errorf("%s was kept: it would load and then fail to run", ref)
		}
	}
	if stored, _ := rows.Images(context.Background(), 7); len(stored) != 0 {
		t.Errorf("recorded %d rows for a refused transfer, want none", len(stored))
	}
}

func TestTransferRefusesWhatItCannotAnswer(t *testing.T) {
	r := oneRelease()
	mover := newMover("arm64", refsOf(r)...)
	cases := map[string]*Transfer{
		"no runtime": {Machine: "m1", Arch: "arm64"},
		"no arch":    {Mover: mover, Machine: "m1"},
	}
	for name, tr := range cases {
		if _, err := tr.Send(context.Background(),
			TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, io.Discard); err == nil {
			t.Errorf("Send with %s: no error", name)
		}
	}
	tr := &Transfer{Mover: mover, Machine: "m1", Arch: "arm64"}
	for name, req := range map[string]TransferRequest{
		"no release":         {Refs: refsOf(r), Arch: "arm64"},
		"no images":          {Release: 7, Arch: "arm64"},
		"an empty reference": {Release: 7, Refs: []string{""}, Arch: "arm64"},
	} {
		if _, err := tr.Send(context.Background(), req, io.Discard); err == nil {
			t.Errorf("Send with %s: no error", name)
		}
	}
	if _, err := tr.Send(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, nil); err == nil {
		t.Error("Send with nowhere to write: no error")
	}

	if _, err := tr.Receive(context.Background(),
		TransferRequest{Release: 7, Refs: refsOf(r), Arch: "arm64"}, strings.NewReader("tar:")); err == nil {
		t.Error("Receive with no store: no error")
	}
}

func TestTransferRefusesAReferenceThatNamesNoService(t *testing.T) {
	mover := newMover("arm64", "postgres:16-alpine")
	tr := &Transfer{Mover: mover, Machine: "m1", Arch: "arm64"}
	_, err := tr.Send(context.Background(),
		TransferRequest{Release: 7, Refs: []string{"postgres:16-alpine"}, Arch: "arm64"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), NamePrefix) {
		t.Fatalf("Send of an image that is not a release's: %v, want the shape named", err)
	}
}
