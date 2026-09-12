package runtime

import (
	"context"
	"fmt"
	"io"
)

type ImageInfo struct {
	Ref string `json:"ref"`

	ID string `json:"id,omitempty"`

	Arch string `json:"arch,omitempty"`

	Size int64 `json:"size,omitempty"`
}

type ImageStreamer interface {
	ImageInfo(ctx context.Context, ref string) (ImageInfo, error)

	SaveImages(ctx context.Context, refs []string, out io.Writer) (int64, error)

	LoadImages(ctx context.Context, in io.Reader) ([]string, error)
}

func Streamer(d Driver) (ImageStreamer, error) {
	if d == nil {
		return nil, fmt.Errorf("this machine has no container runtime, so images cannot be moved to or from it")
	}
	s, ok := d.(ImageStreamer)
	if !ok {
		return nil, fmt.Errorf("this machine's container runtime (%T) cannot save or load images, "+
			"so a release has to be built here rather than copied", d)
	}
	return s, nil
}
