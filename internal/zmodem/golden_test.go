package zmodem_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/testutil"
	"github.com/solvalou/xfer/internal/zmodem"
)

// These tests load the TS-produced goldens from test/golden/ and verify that
// the Go implementation produces byte-identical output for the same scenario.
// They are the contract that keeps the port provably equivalent to the
// original.

func TestGolden_ZfileHelloTxt(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	payload := bytes.Repeat([]byte{0x41}, 42)
	errCh := runSend(server, payload, "hello.txt")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	time.Sleep(20 * time.Millisecond)

	got := cap.Bytes()
	want, err := os.ReadFile(filepath.Join("..", "..", "test", "golden", "zfile-hello-txt.bin"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes differ from committed golden:\n%s", testutil.FirstDiff(want, got))
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}

func TestGolden_ZdataEveryByte(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	payload := make([]byte, 256)
	for i := 0; i < 256; i++ {
		payload[i] = byte(i)
	}
	errCh := runSend(server, payload, "every-byte.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	_, _ = client.Write(zrpos(0))
	cap.WaitFor(t, func(b []byte) bool { return countTerm(b, zmodem.ZCRCW) >= 2 }, 3*time.Second)
	time.Sleep(30 * time.Millisecond)

	got := cap.Bytes()
	want, err := os.ReadFile(filepath.Join("..", "..", "test", "golden", "zdata-every-byte.bin"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes differ from committed golden:\n%s", testutil.FirstDiff(want, got))
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}
