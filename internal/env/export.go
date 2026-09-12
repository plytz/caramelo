package env

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func (m *Manager) Export(ctx context.Context, app, name string, format ExportFormat) (string, error) {
	rec, err := m.env(ctx, app, name)
	if err != nil {
		return "", err
	}
	e, err := recordToEnv(rec)
	if err != nil {
		return "", err
	}
	return render(e.Vars, format)
}

func render(vars map[string]string, format ExportFormat) (string, error) {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	switch format {
	case FormatShell, "":
		for _, k := range keys {
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(vars[k]))
		}
	case FormatDotenv:
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, vars[k])
		}
	case FormatJSON:
		if vars == nil {
			vars = map[string]string{}
		}
		out, err := json.MarshalIndent(vars, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encode variables: %w", err)
		}
		b.Write(out)
		b.WriteString("\n")
	default:
		return "", fmt.Errorf("unknown format %q: want %s, %s or %s", format, FormatShell, FormatDotenv, FormatJSON)
	}
	return b.String(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
