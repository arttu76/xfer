package viewer

import (
	"bytes"
	"strings"
	"testing"
)

// -------------------------------------------------------------
// isBinary — text/hex auto-detection heuristic
// -------------------------------------------------------------

func TestIsBinary_Empty(t *testing.T) {
	if isBinary(nil) {
		t.Fatal("empty data should classify as text")
	}
}

func TestIsBinary_AsciiText(t *testing.T) {
	body := []byte("hello, world\nline two\r\nline three\tcolumn\n")
	if isBinary(body) {
		t.Fatal("plain ASCII with LF/CRLF/TAB should be text")
	}
}

func TestIsBinary_AnyNulByteIsBinary(t *testing.T) {
	// A single NUL anywhere in the 4 KB window flips to binary — this
	// is what distinguishes executables / archives / images from text.
	body := append([]byte("looks mostly like text "), 0x00)
	body = append(body, []byte(" but has a NUL")...)
	if !isBinary(body) {
		t.Fatal("NUL byte in sample must force binary classification")
	}
}

func TestIsBinary_ManyControlCharsIsBinary(t *testing.T) {
	// 20% control bytes beats the 10% threshold — hex mode.
	body := bytes.Repeat([]byte{0x01}, 20)
	body = append(body, bytes.Repeat([]byte("a"), 80)...)
	if !isBinary(body) {
		t.Fatal("20% control-byte content should be binary")
	}
}

func TestIsBinary_HighBitBytesStillText(t *testing.T) {
	// UTF-8, Latin-1, PETSCII all have 0x80+ bytes; we don't want to
	// fling those into hex mode.
	body := []byte("Café — niños \xe2\x80\x94 test\n")
	if isBinary(body) {
		t.Fatal("high-bit bytes (UTF-8) must not trigger binary")
	}
}

func TestIsBinary_OnlySamplesFirst4K(t *testing.T) {
	// Huge text prefix then a NUL outside the sampling window — should
	// still be classified as text.
	body := bytes.Repeat([]byte("x"), 8192)
	body = append(body, 0x00)
	if isBinary(body) {
		t.Fatal("NUL beyond the 4 KB sample must not flip classification")
	}
}

// -------------------------------------------------------------
// indexCI — case-insensitive ASCII search
// -------------------------------------------------------------

func TestIndexCI_FindsCaseInsensitive(t *testing.T) {
	data := []byte("Hello, World!")
	if got := indexCI(data, []byte("world"), 0); got != 7 {
		t.Fatalf("want 7, got %d", got)
	}
	if got := indexCI(data, []byte("HELLO"), 0); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

func TestIndexCI_RespectsStartOffset(t *testing.T) {
	data := []byte("foo bar foo baz foo")
	// First match at 0, next at 8, next at 16.
	if got := indexCI(data, []byte("foo"), 1); got != 8 {
		t.Fatalf("want 8, got %d", got)
	}
	if got := indexCI(data, []byte("foo"), 9); got != 16 {
		t.Fatalf("want 16, got %d", got)
	}
}

func TestIndexCI_NotFound(t *testing.T) {
	if got := indexCI([]byte("abc"), []byte("xyz"), 0); got != -1 {
		t.Fatalf("want -1, got %d", got)
	}
}

func TestIndexCI_EmptyNeedleReturnsNegative(t *testing.T) {
	// Empty-needle behaviour is "don't do anything useful" — the caller
	// in handleSearchInput filters empty strings before calling.
	if got := indexCI([]byte("abc"), nil, 0); got != -1 {
		t.Fatalf("empty needle must return -1, got %d", got)
	}
}

func TestIndexCI_NonAsciiBytesMatchExact(t *testing.T) {
	data := []byte{0xe2, 0x80, 0x94, 'x'}
	needle := []byte{0xe2, 0x80, 0x94}
	if got := indexCI(data, needle, 0); got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

// -------------------------------------------------------------
// rebuildCharLines — line splitting and wrap
// -------------------------------------------------------------

func TestRebuildCharLines_SplitOnLF(t *testing.T) {
	s := &state{data: []byte("abc\ndef\nghi"), width: 80}
	s.rebuildCharLines()
	// Expected line starts: 0, 4 (after first \n), 8 (after second \n).
	want := []int{0, 4, 8}
	if !equalInts(s.charLines, want) {
		t.Fatalf("LF split: got %v, want %v", s.charLines, want)
	}
}

func TestRebuildCharLines_CRLFEquivalentToLF(t *testing.T) {
	lf := &state{data: []byte("abc\ndef\nghi"), width: 80}
	crlf := &state{data: []byte("abc\r\ndef\r\nghi"), width: 80}
	lf.rebuildCharLines()
	crlf.rebuildCharLines()
	if len(lf.charLines) != len(crlf.charLines) {
		t.Fatalf("CRLF should produce same line count as LF: %d vs %d",
			len(crlf.charLines), len(lf.charLines))
	}
}

func TestRebuildCharLines_WrapsAtWidth(t *testing.T) {
	// 25 chars, width 10 → wraps into 3 lines.
	s := &state{data: []byte("aaaaaaaaaabbbbbbbbbbccccc"), width: 10}
	s.rebuildCharLines()
	if len(s.charLines) != 3 {
		t.Fatalf("want 3 wrapped lines, got %d: %v", len(s.charLines), s.charLines)
	}
}

func TestRebuildCharLines_TabExpandsToStops(t *testing.T) {
	// "ab\tcd" renders as "ab      cd" — 2 + 6 (tab to col 8) + 2 = 10 cols.
	// At width 9 the "cd" must wrap to the next line.
	s := &state{data: []byte("ab\tcd"), width: 9}
	s.rebuildCharLines()
	if len(s.charLines) < 2 {
		t.Fatalf("tab should trigger wrap at width 9, got %d lines", len(s.charLines))
	}
}

func TestRebuildCharLines_Empty(t *testing.T) {
	s := &state{data: nil, width: 40}
	s.rebuildCharLines()
	if len(s.charLines) != 1 || s.charLines[0] != 0 {
		t.Fatalf("empty data must produce a single line at offset 0, got %v", s.charLines)
	}
}

// -------------------------------------------------------------
// hexBytesPerLine — must fit in the configured width
// -------------------------------------------------------------

func TestHexBytesPerLine_40colSmallFile(t *testing.T) {
	s := &state{data: make([]byte, 100), width: 40}
	// offsetDigits = 4, overhead = 6, avail = 34, n = 8.
	if got := s.hexBytesPerLine(); got != 8 {
		t.Fatalf("40-col small file: want 8 bytes/line, got %d", got)
	}
}

func TestHexBytesPerLine_40colLargeFile(t *testing.T) {
	// > 64 KB pushes offsetDigits to 6; the tight gutter still allows 8.
	s := &state{data: make([]byte, 0x20000), width: 40}
	if got := s.hexBytesPerLine(); got != 8 {
		t.Fatalf("40-col large file: want 8 bytes/line, got %d", got)
	}
}

func TestHexBytesPerLine_80col(t *testing.T) {
	s := &state{data: make([]byte, 100), width: 80}
	if got := s.hexBytesPerLine(); got != 16 {
		t.Fatalf("80-col: want 16 bytes/line, got %d", got)
	}
}

func TestHexBytesPerLine_TightWidthStillPositive(t *testing.T) {
	// A pathological 20-column terminal still has to render something.
	s := &state{data: make([]byte, 100), width: 20}
	if got := s.hexBytesPerLine(); got < 1 {
		t.Fatalf("tight width: must render at least 1 byte/line, got %d", got)
	}
}

func TestRenderedHexLineFitsInWidth(t *testing.T) {
	// Belt-and-braces: whatever hexBytesPerLine decides, the rendered
	// line must not exceed the configured width.
	for _, width := range []int{24, 40, 60, 80, 100, 132} {
		s := &state{data: []byte("the quick brown fox"), width: width}
		s.mode = ModeHex
		line := s.renderHexLine(0)
		if len(line) > width {
			t.Fatalf("width=%d: rendered %d chars (%q)", width, len(line), line)
		}
	}
}

// -------------------------------------------------------------
// search continuation — lastMatchByte advances past the previous hit
// -------------------------------------------------------------

func TestSearchContinuation_ThroughIndexCI(t *testing.T) {
	// We verify the invariant the viewer relies on without spinning up
	// a connection: given a recorded match at lastMatchByte,
	// indexCI(data, needle, lastMatchByte+1) must return the NEXT hit.
	data := []byte("line 1 foo\nline 2 foo\nline 3 foo\nline 4 end\n")
	needle := []byte("foo")

	firstHit := indexCI(data, needle, 0)
	if firstHit < 0 {
		t.Fatalf("setup: first hit not found")
	}
	secondHit := indexCI(data, needle, firstHit+1)
	if secondHit <= firstHit {
		t.Fatalf("continuation must advance past first hit; first=%d second=%d",
			firstHit, secondHit)
	}
	thirdHit := indexCI(data, needle, secondHit+1)
	if thirdHit <= secondHit {
		t.Fatalf("third hit must advance; second=%d third=%d", secondHit, thirdHit)
	}
	// Sanity: after the last "foo", next search wraps (returns -1 here).
	if idx := indexCI(data, needle, thirdHit+1); idx != -1 {
		t.Fatalf("expected no more matches past %d, got %d", thirdHit, idx)
	}
}

// -------------------------------------------------------------
// setByteTopLine ↔ currentByte round-trip
// -------------------------------------------------------------

func TestCharModeByteToLineRoundTrip(t *testing.T) {
	body := strings.Repeat("line\n", 50) // line[i] starts at byte i*5
	s := &state{data: []byte(body), width: 80}
	s.rebuildCharLines()

	// Point at byte 23 (inside line 4, which starts at byte 20).
	s.setByteTopLine(23)
	if s.topLine != 4 {
		t.Fatalf("want topLine=4, got %d", s.topLine)
	}
	// currentByte returns the line START byte, not the anchor byte — this
	// is the intentional contract used by the search flow.
	if got := s.currentByte(); got != 20 {
		t.Fatalf("currentByte must return line start 20, got %d", got)
	}
}

func TestHexModeByteToLineRoundTrip(t *testing.T) {
	s := &state{data: make([]byte, 256), width: 40, mode: ModeHex}
	// At width 40 / od=4 we get 8 bytes/line. Byte 20 → line 2 (bytes 16-23).
	s.setByteTopLine(20)
	if s.topLine != 2 {
		t.Fatalf("want topLine=2, got %d", s.topLine)
	}
	if got := s.currentByte(); got != 16 {
		t.Fatalf("currentByte want 16, got %d", got)
	}
}

// helpers -----------------------------------------------------

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
