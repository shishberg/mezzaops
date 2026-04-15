package service

import (
	"fmt"
	"unicode/utf8"
)

// TruncateTailToRuneBudget returns s unchanged when its rune count fits
// within budget. Otherwise it keeps the tail of s (the last N runes) and
// prepends a marker line reporting how many bytes of earlier content were
// dropped. The returned string is always valid UTF-8 and its rune count is
// <= budget.
//
// Callers are responsible for subtracting any surrounding scaffolding
// (prefix text, code fences, etc.) from the platform limit before passing
// budget in.
func TruncateTailToRuneBudget(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= budget {
		return s
	}

	// Build the marker first so we know how many runes it consumes. The
	// dropped-byte count depends on which tail we keep, and that depends on
	// the marker's length, so we iterate until a fixed point is reached.
	// In practice one or two iterations suffice because the marker length
	// is bounded by the digit count of the byte total.
	tailBudget := budget
	var marker string
	for i := 0; i < 4; i++ {
		kept := runeSuffix(s, tailBudget)
		dropped := len(s) - len(kept)
		next := fmt.Sprintf("... (truncated, %d earlier bytes omitted)\n", dropped)
		if next == marker {
			break
		}
		marker = next
		markerRunes := utf8.RuneCountInString(marker)
		if markerRunes >= budget {
			// Not enough room for marker plus any tail content. Return
			// just the tail runes so we at least stay within budget.
			return runeSuffix(s, budget)
		}
		tailBudget = budget - markerRunes
	}

	return marker + runeSuffix(s, budget-utf8.RuneCountInString(marker))
}

// runeSuffix returns the last n runes of s, or s itself if s has fewer
// runes than n. It never splits a multibyte rune.
func runeSuffix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	total := utf8.RuneCountInString(s)
	if total <= n {
		return s
	}
	skip := total - n
	i := 0
	for skip > 0 {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		skip--
	}
	return s[i:]
}
