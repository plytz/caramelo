package serverconfig

import "github.com/plytz/caramelo/internal/userdir"

func init() {
	userdir.ServerConfigured = func() bool { return Exists(ConfigDir()) }
}
