package config

import "strings"

type KnownImage struct {
	Image string `json:"image"`

	Port int `json:"port"`

	Ready []string `json:"ready,omitempty"`

	Env map[string]string `json:"env,omitempty"`

	Data string `json:"data,omitempty"`

	PasswordVar string `json:"password_var,omitempty"`

	PasswordChange string `json:"password_change,omitempty"`
}

func (k KnownImage) Password() (string, bool) {
	if k.PasswordVar == "" {
		return "", false
	}
	v, ok := k.Env[k.PasswordVar]
	return v, ok
}

type KnownImageTable []KnownImage

func (t KnownImageTable) Lookup(image string) (KnownImage, bool) {
	name := ImageName(image)
	if name == "" {
		return KnownImage{}, false
	}
	for _, k := range t {
		if k.Image != name {
			continue
		}
		out := k
		out.Ready = append([]string(nil), k.Ready...)
		out.Env = mergeVars(k.Env, nil)
		return out, true
	}
	return KnownImage{}, false
}

func ImageName(ref string) string {
	s := strings.TrimSpace(ref)
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

var Known = KnownImageTable{
	{
		Image:          "postgres",
		Port:           5432,
		Ready:          []string{"pg_isready", "-U", "postgres"},
		Env:            map[string]string{"POSTGRES_PASSWORD": "caramelo"},
		PasswordVar:    "POSTGRES_PASSWORD",
		PasswordChange: `ALTER USER postgres WITH PASSWORD '<the new value>'`,
		Data:           "/var/lib/postgresql/data",
	},
	{
		Image: "redis",
		Port:  6379,
		Ready: []string{"redis-cli", "ping"},
		Data:  "/data",
	},
	{
		Image:          "mysql",
		Port:           3306,
		Ready:          []string{"mysqladmin", "ping"},
		Env:            map[string]string{"MYSQL_ROOT_PASSWORD": "caramelo"},
		PasswordVar:    "MYSQL_ROOT_PASSWORD",
		PasswordChange: `ALTER USER 'root'@'%' IDENTIFIED BY '<the new value>'`,
		Data:           "/var/lib/mysql",
	},
	{
		Image:          "mariadb",
		Port:           3306,
		Ready:          []string{"mysqladmin", "ping"},
		Env:            map[string]string{"MYSQL_ROOT_PASSWORD": "caramelo"},
		PasswordVar:    "MYSQL_ROOT_PASSWORD",
		PasswordChange: `ALTER USER 'root'@'%' IDENTIFIED BY '<the new value>'`,
		Data:           "/var/lib/mysql",
	},
	{
		Image: "mongo",
		Port:  27017,
		Ready: []string{"mongosh", "--eval", "db.runCommand('ping')"},
		Data:  "/data/db",
	},
	{
		Image: "rabbitmq",
		Port:  5672,
		Ready: []string{"rabbitmq-diagnostics", "check_port_connectivity"},
		Data:  "/var/lib/rabbitmq",
	},
}

func Lookup(image string) (KnownImage, bool) { return Known.Lookup(image) }
