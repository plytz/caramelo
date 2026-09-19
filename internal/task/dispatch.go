package task

import "strings"

const Binary = "caramelo"

const Metacharacters = "|&;<>()$`\\*?[]#~=%\n"

func InProcess(script string) ([]string, bool) {
	if strings.ContainsAny(script, Metacharacters) {
		return nil, false
	}
	argv, ok := splitArgv(script)
	if !ok || len(argv) == 0 || argv[0] != Binary {
		return nil, false
	}
	return argv, true
}

func splitArgv(s string) ([]string, bool) {
	var (
		out   []string
		cur   strings.Builder
		word  bool
		quote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote, word = c, true
		case c == ' ' || c == '\t':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteByte(c)
			word = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if word {
		out = append(out, cur.String())
	}
	return out, true
}
