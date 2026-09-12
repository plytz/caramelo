package vpnclient

import (
	"os"
	"path/filepath"
	"strings"
)

var mountsFile = "/proc/self/mounts"

func nosuidMount(path string) string {
	b, err := os.ReadFile(mountsFile)
	if err != nil {
		return ""
	}
	abs := path
	if a, err := filepath.Abs(path); err == nil {
		abs = a
	}
	best, bestOpts := "", ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		point, opts := unescapeMount(fields[1]), fields[3]
		if !underMount(abs, point) {
			continue
		}
		if len(point) >= len(best) {
			best, bestOpts = point, opts
		}
	}
	if best == "" {
		return ""
	}
	for _, opt := range strings.Split(bestOpts, ",") {
		if opt == "nosuid" {
			return best
		}
	}
	return ""
}

func underMount(path, point string) bool {
	if point == "/" {
		return true
	}
	return path == point || strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/")
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
