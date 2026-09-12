package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runtime"
)

type Mover interface {
	ImageInfo(ctx context.Context, ref string) (runtime.ImageInfo, error)
	SaveImages(ctx context.Context, refs []string, out io.Writer) (int64, error)
	LoadImages(ctx context.Context, in io.Reader) ([]string, error)
	RemoveImage(ctx context.Context, ref string) error
}

type Transfer struct {
	Mover Mover

	Store ImageStore

	Machine string
	Arch    string

	Now func() time.Time
}

type TransferRequest struct {
	Release int64    `json:"release"`
	Refs    []string `json:"refs"`
	Arch    string   `json:"arch"`
}

type TransferResult struct {
	Images Images `json:"images,omitempty"`

	Bytes    int64         `json:"bytes,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
}

func (t *Transfer) Send(ctx context.Context, req TransferRequest, out io.Writer) (*TransferResult, error) {
	refs, err := t.check(req)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("send release %d from %s: nowhere to write the images to",
			req.Release, where(t.Machine))
	}
	images := make(Images, 0, len(refs))
	for _, ref := range refs {
		info, err := t.info(ctx, ref)
		if err != nil {
			return nil, err
		}
		row, err := t.rowOf(req.Release, ref, info)
		if err != nil {
			return nil, err
		}
		images = append(images, row)
	}
	started := t.now()
	n, err := t.Mover.SaveImages(ctx, refs, out)
	if err != nil {
		return nil, fmt.Errorf("send %s of release %d from %s: %w",
			plural(len(refs), "image"), req.Release, where(t.Machine), err)
	}
	images.Sort()
	return &TransferResult{Images: images, Bytes: n, Duration: t.now().Sub(started)}, nil
}

func (t *Transfer) Receive(ctx context.Context, req TransferRequest, in io.Reader) (*TransferResult, error) {
	refs, err := t.check(req)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("receive release %d on %s: nothing to read the images from",
			req.Release, where(t.Machine))
	}
	if t.Store == nil {
		return nil, fmt.Errorf("receive release %d on %s: this machine records no images, "+
			"so a copy could not be found again", req.Release, where(t.Machine))
	}
	counted := &countingReader{r: in}
	started := t.now()
	loaded, err := t.Mover.LoadImages(ctx, counted)
	if err != nil {
		return nil, fmt.Errorf("receive %s of release %d on %s: %w",
			plural(len(refs), "image"), req.Release, where(t.Machine), err)
	}
	elapsed := t.now().Sub(started)
	arrived := map[string]bool{}
	for _, ref := range loaded {
		arrived[ref] = true
	}
	if missing := notIn(refs, arrived); len(missing) > 0 {
		t.undo(ctx, loaded)
		return nil, fmt.Errorf("receive release %d on %s: the archive carried %s and not %s",
			req.Release, where(t.Machine), quotedOrNothing(loaded), quoted(missing))
	}
	images := make(Images, 0, len(refs))
	for _, ref := range refs {
		info, err := t.info(ctx, ref)
		if err != nil {
			t.undo(ctx, loaded)
			return nil, err
		}
		row, err := t.rowOf(req.Release, ref, info)
		if err != nil {
			t.undo(ctx, loaded)
			return nil, err
		}
		images = append(images, row)
	}
	images.Sort()
	for _, row := range images {
		if err := t.Store.PutImage(ctx, row); err != nil {
			return nil, fmt.Errorf("record image %s of release %d on %s: %w",
				row.Ref, req.Release, where(t.Machine), err)
		}
	}
	return &TransferResult{Images: images, Bytes: counted.n, Duration: elapsed}, nil
}

func (t *Transfer) check(req TransferRequest) ([]string, error) {
	if t.Mover == nil {
		return nil, fmt.Errorf("move the images of release %d: %s has no container runtime",
			req.Release, where(t.Machine))
	}
	if req.Release == 0 {
		return nil, errors.New("move images: no release given")
	}
	if strings.TrimSpace(t.Arch) == "" {
		return nil, fmt.Errorf("move the images of release %d: %s has not said what architecture it is",
			req.Release, where(t.Machine))
	}
	if req.Arch != "" && req.Arch != t.Arch {

		return nil, fmt.Errorf("the images of release %d are %s and %s is %s: "+
			"a release travels only between machines of one architecture",
			req.Release, req.Arch, where(t.Machine), t.Arch)
	}
	refs := make([]string, 0, len(req.Refs))
	for _, ref := range req.Refs {
		if strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("move the images of release %d: an empty image reference", req.Release)
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("move the images of release %d: no images given", req.Release)
	}
	return refs, nil
}

func (t *Transfer) info(ctx context.Context, ref string) (runtime.ImageInfo, error) {
	info, err := t.Mover.ImageInfo(ctx, ref)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return info, fmt.Errorf("image %s: %s does not hold it: %w", ref, where(t.Machine), err)
	case err != nil:
		return info, fmt.Errorf("look for image %s on %s: %w", ref, where(t.Machine), err)
	}
	return info, nil
}

func (t *Transfer) rowOf(id int64, ref string, info runtime.ImageInfo) (Image, error) {
	if info.Arch != "" && info.Arch != t.Arch {
		return Image{}, fmt.Errorf("image %s is %s and %s is %s: it would load and then fail to run",
			ref, info.Arch, where(t.Machine), t.Arch)
	}
	service := ServiceOf(ref)
	if service == "" {
		return Image{}, fmt.Errorf("image %s is not one of Caramelo's release images "+
			"(%s/<app>/<service>:<tree>), so nothing can say which service it is", ref, NamePrefix)
	}
	return Image{
		ReleaseID: id, Service: service, Ref: ref, Arch: t.Arch,
		Machine: t.Machine, ID: info.ID, BuiltAt: t.now(),
	}, nil
}

func (t *Transfer) undo(ctx context.Context, refs []string) {
	for _, ref := range refs {

		_ = t.Mover.RemoveImage(ctx, ref)
	}
}

func (t *Transfer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func notIn(refs []string, arrived map[string]bool) []string {
	var out []string
	for _, ref := range refs {
		if !arrived[ref] {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

func quotedOrNothing(names []string) string {
	if len(names) == 0 {
		return "nothing"
	}
	return quoted(names)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
