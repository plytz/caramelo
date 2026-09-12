package docker

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/runtime"
)

var _ runtime.ImageStreamer = (*CLI)(nil)

const imageInfoFormat = "{{.Id}}\t{{.Architecture}}\t{{.Size}}"

func (c *CLI) ImageInfo(ctx context.Context, ref string) (runtime.ImageInfo, error) {
	if ref == "" {
		return runtime.ImageInfo{}, fmt.Errorf("docker image inspect: no image given")
	}
	if err := checkImage(ref); err != nil {
		return runtime.ImageInfo{}, err
	}
	args := []string{"image", "inspect", "--format", imageInfoFormat, ref}
	res, err := c.run(ctx, args...)
	if err != nil {
		return runtime.ImageInfo{}, err
	}
	if res.ExitCode != 0 {
		if isNoSuchImage(res) {
			return runtime.ImageInfo{}, fmt.Errorf("image %s: %w", ref, runtime.ErrNotFound)
		}
		return runtime.ImageInfo{}, cmdErr(args, res)
	}
	return parseImageInfo(ref, res.Stdout), nil
}

func parseImageInfo(ref, out string) runtime.ImageInfo {
	info := runtime.ImageInfo{Ref: ref}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) > 0 {
		info.ID = strings.TrimSpace(fields[0])
	}
	if len(fields) > 1 {
		info.Arch = NormaliseArch(fields[1])
	}
	if len(fields) > 2 {
		if n, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil {
			info.Size = n
		}
	}
	return info
}

func NormaliseArch(s string) string {
	switch a := strings.ToLower(strings.TrimSpace(s)); a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64", "arm64/v8":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	case "i386", "i686", "x86":
		return "386"
	default:
		return a
	}
}

func (c *CLI) SaveImages(ctx context.Context, refs []string, out io.Writer) (int64, error) {
	if len(refs) == 0 {
		return 0, fmt.Errorf("docker save: no images given")
	}
	if out == nil {
		return 0, fmt.Errorf("docker save: nowhere to write the images to")
	}
	args := []string{"save"}
	for _, ref := range refs {
		if ref == "" {
			return 0, fmt.Errorf("docker save: an empty image reference")
		}
		if err := checkImage(ref); err != nil {
			return 0, err
		}
		args = append(args, ref)
	}
	counted := &countingWriter{w: out}
	var errTail lastBytes
	cmd := runner.Cmd{Name: "docker", Args: args, User: c.User, Stdout: counted, Stderr: &errTail}
	res, err := c.exec(ctx, cmd)
	if err != nil {
		return counted.n, err
	}
	if res.ExitCode != 0 {
		res.Stderr = errTail.String()
		return counted.n, cmdErr(args, res)
	}
	return counted.n, nil
}

func (c *CLI) LoadImages(ctx context.Context, in io.Reader) ([]string, error) {
	if in == nil {
		return nil, fmt.Errorf("docker load: nothing to read the images from")
	}
	args := []string{"load"}
	cmd := runner.Cmd{Name: "docker", Args: args, User: c.User, Stdin: in}
	res, err := c.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, cmdErr(args, res)
	}
	return loadedRefs(res.Stdout + "\n" + res.Stderr), nil
}

func loadedRefs(out string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "Loaded image: ")
		if !ok {
			continue
		}
		ref := strings.TrimSpace(rest)
		if ref != "" {
			seen[ref] = true
		}
	}
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
