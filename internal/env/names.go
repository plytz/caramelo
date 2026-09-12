package env

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	MaxSlugLen = 32

	SlugPattern = `^[a-z0-9][a-z0-9-]{0,31}$`
)

var slugRe = regexp.MustCompile(SlugPattern)

func ValidSlug(s string) bool { return slugRe.MatchString(s) }

func ValidateName(kind, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s name is required", kind)
	case len(name) > MaxSlugLen:
		return fmt.Errorf("invalid %s name %q: at most %d characters", kind, name, MaxSlugLen)
	case !ValidSlug(name):
		return fmt.Errorf("invalid %s name %q: use lowercase letters, digits and dashes, starting with a letter or a digit", kind, name)
	}
	return nil
}

const (
	LabelApp     = "caramelo.app"
	LabelEnv     = "caramelo.env"
	LabelDep     = "caramelo.dep"
	LabelVersion = "caramelo.version"

	LabelService = "caramelo.service"

	LabelTree = "caramelo.tree"
)

const NamePrefix = "caramelo"

func ContainerName(app, env, dep string) string {
	return strings.Join([]string{NamePrefix, app, env, dep}, "-")
}

func ServiceContainerName(app, env, service string) string {
	return ContainerName(app, env, service)
}

func NetworkName(app, env string) string {
	return strings.Join([]string{NamePrefix, app, env}, "-")
}

const cacheSuffix = "--cache"

func CacheVolumeName(app, env string) string {
	return NetworkName(app, env) + cacheSuffix
}

func ImageRef(app, tag string) string {
	return NamePrefix + "/" + app + ":" + tag
}

func VolumeName(app, env, dep string) string { return ContainerName(app, env, dep) }

func Labels(app, env, dep, version string) map[string]string {
	l := map[string]string{LabelApp: app, LabelEnv: env}
	if dep != "" {
		l[LabelDep] = dep
	}
	if version != "" {
		l[LabelVersion] = version
	}
	return l
}

func ServiceLabels(app, env, service, version string) map[string]string {
	l := Labels(app, env, "", version)
	if service != "" {
		l[LabelService] = service
	}
	return l
}

func ReplicaLabels(app, env, service, version, tree string) map[string]string {
	l := ServiceLabels(app, env, service, version)
	if tree != "" {
		l[LabelTree] = tree
	}
	return l
}

func AppsDir(data string) string { return filepath.Join(data, "apps") }

func AppDir(data, app string) string { return filepath.Join(AppsDir(data), app) }

func RepoPath(data, app string) string { return filepath.Join(AppDir(data, app), "repo.git") }

func EnvsDir(data, app string) string { return filepath.Join(AppDir(data, app), "envs") }

func EnvDir(data, app, env string) string { return filepath.Join(EnvsDir(data, app), env) }

func WorktreePath(data, app, env string) string { return filepath.Join(EnvDir(data, app, env), "src") }

func AppOfRepo(path string) string {
	dir := strings.TrimSuffix(strings.TrimRight(filepath.Clean(path), "/"), ".git")
	base := filepath.Base(dir)
	if base == "repo" {
		return filepath.Base(filepath.Dir(dir))
	}
	return base
}
