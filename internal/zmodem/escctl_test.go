package zmodem_test

import (
	"testing"

	"github.com/solvalou/xfer/internal/zmodem"
)

func TestEscctlSniff_TooShort(t *testing.T) {
	if _, ok := zmodem.DetectEscctlFromZrinit(nil); ok {
		t.Fatal("expected ok=false for empty input")
	}
	if _, ok := zmodem.DetectEscctlFromZrinit([]byte{0x2a, 0x2a, 0x18, 0x42}); ok {
		t.Fatal("expected ok=false for too-short input")
	}
}

func TestEscctlSniff_Term48(t *testing.T) {
	got, ok := zmodem.DetectEscctlFromZrinit(zrinit(capsTerm48))
	if !ok {
		t.Fatal("failed to detect ZRINIT in Term 4.8 sample")
	}
	if got != false {
		t.Fatal("Term 4.8 caps should NOT request ESCCTL")
	}
}

func TestEscctlSniff_LrzszEscape(t *testing.T) {
	got, ok := zmodem.DetectEscctlFromZrinit(zrinit(capsLrzszEscape))
	if !ok {
		t.Fatal("failed to detect ZRINIT in lrzsz --escape sample")
	}
	if !got {
		t.Fatal("lrzsz --escape caps should request ESCCTL")
	}
}

func TestEscctlSniff_NonZrinitHexFrame(t *testing.T) {
	// A valid hex frame with type=0x03 (ZACK). Must NOT be misread as ZRINIT.
	ack := zmodem.BuildZhexHeader(zmodem.FrameZACK, 0)
	if _, ok := zmodem.DetectEscctlFromZrinit(ack); ok {
		t.Fatal("non-ZRINIT hex frame leaked through the sniff")
	}
}

func TestEscctlSniff_JunkLeading(t *testing.T) {
	in := append([]byte{0x00, 0x01, 0x02, 0x03}, zrinit(capsLrzszEscape)...)
	got, ok := zmodem.DetectEscctlFromZrinit(in)
	if !ok || !got {
		t.Fatalf("failed on leading-junk input: ok=%v got=%v", ok, got)
	}
}

func TestEscctlSniff_Chunked(t *testing.T) {
	full := zrinit(capsLrzszEscape)
	var sniff []byte
	var last bool
	var found bool
	for _, b := range full {
		sniff = append(sniff, b)
		if len(sniff) > 24 {
			sniff = sniff[len(sniff)-24:]
		}
		if r, ok := zmodem.DetectEscctlFromZrinit(sniff); ok {
			last = r
			found = true
		}
	}
	if !found || !last {
		t.Fatal("chunked sniff missed ESCCTL")
	}
}

func TestZdleTableDefault(t *testing.T) {
	zmodem.DisableEscctl() // make sure we measure the default state
	expected := map[byte]byte{
		0x0d: 0x4d, 0x10: 0x50, 0x11: 0x51, 0x13: 0x53, 0x18: 0x58, 0x7f: 0x6c,
		0x8d: 0xcd, 0x90: 0xd0, 0x91: 0xd1, 0x93: 0xd3, 0xff: 0x6d,
	}
	for b := 0; b < 256; b++ {
		got := zmodem.ZdleTable[b]
		if want, ok := expected[byte(b)]; ok {
			if got != want {
				t.Errorf("escape(%#02x): got %#02x want %#02x", b, got, want)
			}
		} else if got != byte(b) {
			t.Errorf("byte %#02x should be identity, got %#02x", b, got)
		}
	}
}

func TestZdleTableEscctlOn(t *testing.T) {
	zmodem.EnableEscctl()
	defer zmodem.DisableEscctl()
	// Control bytes in 0x01..0x1f that weren't already escaped become so.
	if zmodem.ZdleTable[0x05] != 0x45 {
		t.Fatalf("0x05 should escape to 0x45 with ESCCTL on, got %#x", zmodem.ZdleTable[0x05])
	}
	if zmodem.ZdleTable[0x85] != 0xc5 {
		t.Fatalf("0x85 should escape to 0xc5 with ESCCTL on, got %#x", zmodem.ZdleTable[0x85])
	}
	// Always-escaped bytes remain their canonical escape values.
	if zmodem.ZdleTable[0x11] != 0x51 {
		t.Fatalf("0x11 canonical escape changed: got %#x", zmodem.ZdleTable[0x11])
	}
}
