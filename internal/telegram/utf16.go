package telegram

import (
	"fmt"
	"unicode/utf8"
)

// utf16Len returns the length of s in UTF-16 code units, which is how Telegram
// counts message length: characters outside the BMP (most emoji) count as 2.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Units(r)
	}
	return n
}

func utf16Units(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// truncateTailUTF16 is service.TruncateTailToRuneBudget measured in UTF-16
// code units: it keeps the tail of s (where the real error usually is) and
// prefixes a marker saying how much was dropped, so that the result is at most
// budget units. It returns s unchanged if it already fits, and "" if budget
// leaves no room for the marker plus any content.
func truncateTailUTF16(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if utf16Len(s) <= budget {
		return s
	}
	// The marker names the number of dropped bytes. len(s) bounds that
	// number, so a marker built from it is never shorter than the real one.
	markerUnits := utf16Len(fmt.Sprintf("... (truncated, %d earlier bytes omitted)\n", len(s)))
	tailBudget := budget - markerUnits
	if tailBudget <= 0 {
		return ""
	}
	kept := utf16Suffix(s, tailBudget)
	return fmt.Sprintf("... (truncated, %d earlier bytes omitted)\n%s", len(s)-len(kept), kept)
}

// utf16Suffix returns the longest suffix of s, on a rune boundary, that is at
// most units UTF-16 code units long.
func utf16Suffix(s string, units int) string {
	start := len(s)
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		units -= utf16Units(r)
		if units < 0 {
			break
		}
		start -= size
	}
	return s[start:]
}
