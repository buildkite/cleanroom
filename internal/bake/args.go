package bake

import "strings"

// QuoteArgs renders arguments as one shell-quoted line. POSIX shells do not
// apply quote removal to unquoted $(...) substitution, so output containing
// quoted values (stamp annotations, paths with spaces) must be consumed via
// eval: eval "spore create name $(cleanroom compile .) $(cleanroom stamp .)".
// Compile output is safe in bare substitution only because its tokens never
// leave the unquoted-safe character set.
func QuoteArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, quoteArg(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteArg(arg string) string {
	if arg != "" && !strings.ContainsFunc(arg, needsQuoting) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	switch r {
	case '@', '%', '+', '=', ':', ',', '.', '/', '_', '-':
		return false
	}
	return true
}
