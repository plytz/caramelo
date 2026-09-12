package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

func (c *CLI) RemoveImage(ctx context.Context, ref string) error {
	if ref == "" {
		return fmt.Errorf("docker image rm: no image given")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("docker image rm: %q is not an image reference", ref)
	}
	args := []string{"image", "rm", ref}
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !isNoSuchImage(res) && !isImageInUse(res) {
		return cmdErr(args, res)
	}
	return nil
}

func isNoSuchImage(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "no such image") || strings.Contains(s, "reference does not exist")
}

func isImageInUse(res runner.Result) bool {
	s := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(s, "image is being used") || strings.Contains(s, "is using its referenced image")
}
