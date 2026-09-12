package remote

import (
	"fmt"
	"strings"
)

func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsQuoting(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func needsQuoting(s string) bool {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"@%_-+=:,./"
	for _, r := range s {
		if !strings.ContainsRune(safe, r) {
			return true
		}
	}
	return false
}

func QuoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Quote(a)
	}
	return out
}

func JoinArgs(args []string) string {
	return strings.Join(QuoteArgs(args), " ")
}

func SplitWords(s string) ([]string, error) {
	var (
		words  []string
		cur    []rune
		inWord bool
		quote  rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(rs):
				i++
				cur = append(cur, rs[i])
			default:
				cur = append(cur, c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\':
			if i+1 >= len(rs) {
				return nil, fmt.Errorf("trailing backslash in %q", s)
			}
			i++
			cur, inWord = append(cur, rs[i]), true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, string(cur))
				cur, inWord = cur[:0], false
			}
		default:
			cur, inWord = append(cur, c), true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if inWord {
		words = append(words, string(cur))
	}
	return words, nil
}
