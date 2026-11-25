package config

import (
	"regexp/syntax"
	"strings"
)

// literalNumber reports whether a number pattern matches exactly one string
// (such as ^447700900123$), and which.
func literalNumber(pattern string) (string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	parts := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		parts = re.Sub
	}
	if len(parts) < 2 || parts[0].Op != syntax.OpBeginText || parts[len(parts)-1].Op != syntax.OpEndText {
		return "", false
	}
	var b strings.Builder
	for _, s := range parts[1 : len(parts)-1] {
		switch {
		case s.Op == syntax.OpLiteral && s.Flags&syntax.FoldCase == 0:
			b.WriteString(string(s.Rune))
		case s.Op == syntax.OpCapture && len(s.Sub) == 1 && s.Sub[0].Op == syntax.OpLiteral && s.Sub[0].Flags&syntax.FoldCase == 0:
			b.WriteString(string(s.Sub[0].Rune))
		default:
			return "", false
		}
	}
	return b.String(), true
}

// exampleNumber returns a string the pattern matches, preferring digits, so
// that "does an earlier rule also match this?" can be asked of any rule. It is
// one example, not every possible match.
func exampleNumber(pattern string) (string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	if !gen(re.Simplify(), &b) {
		return "", false
	}
	return b.String(), true
}

func gen(re *syntax.Regexp, b *strings.Builder) bool {
	switch re.Op {
	case syntax.OpLiteral:
		b.WriteString(string(re.Rune))
	case syntax.OpCharClass:
		r, ok := pickRune(re.Rune)
		if !ok {
			return false
		}
		b.WriteRune(r)
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		b.WriteByte('0')
	case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary, syntax.OpEmptyMatch:
	case syntax.OpCapture:
		return gen(re.Sub[0], b)
	case syntax.OpConcat:
		for _, s := range re.Sub {
			if !gen(s, b) {
				return false
			}
		}
	case syntax.OpAlternate:
		return gen(re.Sub[0], b)
	case syntax.OpStar, syntax.OpQuest:
	case syntax.OpPlus:
		return gen(re.Sub[0], b)
	case syntax.OpRepeat:
		for i := 0; i < re.Min; i++ {
			if !gen(re.Sub[0], b) {
				return false
			}
		}
	default:
		return false
	}
	return true
}

// pickRune picks a rune from a character class (pairs of lo, hi), preferring
// a digit, then the lowest printable rune.
func pickRune(ranges []rune) (rune, bool) {
	for i := 0; i+1 < len(ranges); i += 2 {
		lo, hi := ranges[i], ranges[i+1]
		if lo <= '9' && hi >= '0' {
			return max(lo, '0'), true
		}
	}
	for i := 0; i+1 < len(ranges); i += 2 {
		if ranges[i+1] >= ' ' {
			return max(ranges[i], ' '), true
		}
	}
	return 0, false
}
