package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateTailToRuneBudget_ShortInputPassesThrough(t *testing.T) {
	in := "hello world"
	got := TruncateTailToRuneBudget(in, 100)
	if got != in {
		t.Fatalf("expected input returned unchanged; got %q", got)
	}
}

func TestTruncateTailToRuneBudget_ExactlyAtLimit(t *testing.T) {
	in := strings.Repeat("a", 50)
	got := TruncateTailToRuneBudget(in, 50)
	if got != in {
		t.Fatalf("expected input returned unchanged at exact limit; got len=%d", utf8.RuneCountInString(got))
	}
}

func TestTruncateTailToRuneBudget_LongInputTruncatedToBudget(t *testing.T) {
	in := strings.Repeat("x", 10000)
	budget := 500
	got := TruncateTailToRuneBudget(in, budget)
	if runes := utf8.RuneCountInString(got); runes > budget {
		t.Fatalf("result rune count %d exceeds budget %d", runes, budget)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker containing 'truncated'; got prefix %q", got[:min(len(got), 100)])
	}
}

func TestTruncateTailToRuneBudget_KeepsTail(t *testing.T) {
	// Build a distinctive head marker that must not survive truncation,
	// and a distinctive tail marker that must.
	headMarker := "HEAD_OF_INPUT_SHOULD_BE_DROPPED"
	tailMarker := "DISTINCTIVE_TAIL_MARKER_END"
	in := headMarker + strings.Repeat("x", 5000) + tailMarker
	got := TruncateTailToRuneBudget(in, 200)
	if !strings.Contains(got, tailMarker) {
		t.Fatalf("expected tail %q preserved in truncated output; got %q", tailMarker, got)
	}
	if strings.Contains(got, headMarker) {
		t.Fatalf("head leaked into truncated output: %q", got)
	}
}

func TestTruncateTailToRuneBudget_MarkerReportsDroppedBytes(t *testing.T) {
	in := strings.Repeat("x", 10000)
	budget := 500
	got := TruncateTailToRuneBudget(in, budget)
	// Marker should include a numeric byte count matching (len(in) - len(kept tail)).
	// Kept tail is whatever follows the marker. We can compute expected.
	// Find end of first line (marker line terminates with \n per our contract).
	idx := strings.IndexByte(got, '\n')
	if idx < 0 {
		t.Fatalf("expected marker terminated by newline; got %q", got)
	}
	kept := got[idx+1:]
	dropped := len(in) - len(kept)
	wantSubstr := "omitted"
	if !strings.Contains(got[:idx], wantSubstr) {
		t.Fatalf("marker missing %q substring: %q", wantSubstr, got[:idx])
	}
	// Reported count must match actual dropped bytes.
	wantNum := itoa(dropped)
	if !strings.Contains(got[:idx], wantNum) {
		t.Fatalf("marker %q does not report dropped byte count %d", got[:idx], dropped)
	}
}

func TestTruncateTailToRuneBudget_MultibyteBoundarySafe(t *testing.T) {
	// Use a 3-byte rune so that naive byte-based slicing at a non-rune
	// boundary would produce invalid UTF-8.
	in := strings.Repeat("世", 2000) // 2000 runes, 6000 bytes
	budget := 100
	got := TruncateTailToRuneBudget(in, budget)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8")
	}
	if runes := utf8.RuneCountInString(got); runes > budget {
		t.Fatalf("rune count %d exceeds budget %d", runes, budget)
	}
	// Tail should still be made of 世 runes (no split).
	// Strip marker line.
	if idx := strings.IndexByte(got, '\n'); idx >= 0 {
		kept := got[idx+1:]
		for _, r := range kept {
			if r != '世' {
				t.Fatalf("unexpected rune %q in kept tail; expected only 世", r)
			}
		}
	}
}

func TestTruncateTailToRuneBudget_TinyBudget(t *testing.T) {
	// When budget is too small even for the marker, the helper should still
	// return something that fits.
	in := strings.Repeat("x", 1000)
	got := TruncateTailToRuneBudget(in, 10)
	if runes := utf8.RuneCountInString(got); runes > 10 {
		t.Fatalf("rune count %d exceeds tiny budget 10", runes)
	}
}

// itoa avoids pulling strconv just for test assertions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
