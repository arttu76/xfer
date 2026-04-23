package session

import (
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solvalou/xfer/internal/kermit"
	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/xmodem"
	"github.com/solvalou/xfer/internal/zmodem"
)

// Note: we can't import navigator or protocol here because they import
// session — the per-connection state machine below is therefore invoked
// *from* those packages via HandleConnection in cmd/xfer/main.go.

// All three transfer functions share a common shape: set mode, announce
// the file with MD5 / size, then hand the socket to the protocol sender.
// The prelude below captures that shared piece and is the single place
// that emits the "Ready to download / Size / MD5 / Initiating" banner.

// OnDone is the completion callback for every transfer. exitCode follows
// the XMODEM convention (0 = success, non-zero = failure); for ZMODEM
// and Kermit we use 0/1 since they don't have a protocol-level code.
type OnDone func(success bool, exitCode int)

// transferPrelude sets transfer mode, reads the file, writes the common
// banner, and returns the file bytes plus basename. On error the caller's
// onDone is invoked (when non-nil) and err is returned so the transfer
// function can bail out.
func transferPrelude(ctx *Context, protoName string, onDone OnDone) ([]byte, string, error) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")

	if ctx.RequestedFile == "" {
		_ = ctx.Writeln("Error: No file selected for transfer")
		if onDone != nil {
			onDone(false, -1)
		}
		return nil, "", errors.New("no file selected for transfer")
	}

	data, err := os.ReadFile(ctx.RequestedFile)
	if err != nil {
		logger.TransferStatus(protoName, fmt.Sprintf("Error reading file: %v", err))
		_ = ctx.Writeln(fmt.Sprintf("Error reading file: %v", err))
		if onDone != nil {
			onDone(false, -1)
		}
		return nil, "", err
	}

	name := filepath.Base(ctx.RequestedFile)
	_ = ctx.Writeln(fmt.Sprintf("Ready to download %s", name))
	_ = ctx.Writeln(fmt.Sprintf("Size: %s", humanBytes(len(data))))
	_ = ctx.Writeln(fmt.Sprintf("MD5:  %x", md5.Sum(data)))
	_ = ctx.Writeln(fmt.Sprintf("Initiating %s transfer for %s", protoName, ctx.RequestedFile))
	return data, name, nil
}

// XmodemTransfer reads the requested file and pushes it to the client via
// XMODEM. Emits structured progress log lines every few seconds.
func XmodemTransfer(ctx *Context, _ *Config, onDone OnDone) {
	data, _, err := transferPrelude(ctx, "XMODEM", onDone)
	if err != nil {
		return
	}

	blocks := (len(data) + xmodem.BlockSize - 1) / xmodem.BlockSize
	if blocks == 0 {
		blocks = 1
	}
	_ = ctx.Writeln(fmt.Sprintf("Please start your XMODEM receiver NOW for %d blocks.", blocks))

	startedAt := time.Now()
	lastLoggedAt := startedAt

	cfg := xmodem.Config{
		ReadTimeout: 10 * time.Second,
		OnStart: func() {
			logger.TransferStatus("XMODEM", fmt.Sprintf("Transfer started for %s", ctx.RequestedFile))
		},
		OnStatus: func(evt xmodem.StatusEvent) {
			now := time.Now()
			if now.Sub(lastLoggedAt) < 5*time.Second || evt.Block == 0 {
				return
			}
			elapsed := now.Sub(startedAt)
			remaining := evt.TotalBlocks - evt.Block
			msPerBlock := float64(elapsed.Milliseconds()) / float64(evt.Block)
			secsLeft := int(float64(remaining) * msPerBlock / 1000)
			bps := int(float64(evt.Block*xmodem.BlockSize) / elapsed.Seconds())
			etaStr := fmt.Sprintf("%dsec", secsLeft%60)
			if m := secsLeft / 60; m > 0 {
				etaStr = fmt.Sprintf("%dmin %s", m, etaStr)
			}
			logger.TransferStatus("XMODEM",
				fmt.Sprintf("Transferred %d of %d blocks - %dB/sec - ETA: %s",
					evt.Block, evt.TotalBlocks, bps, etaStr))
			lastLoggedAt = now
		},
	}

	err = xmodem.Send(connAdapter{ctx.Conn}, data, cfg)
	success := err == nil
	code := 0
	if err != nil {
		logger.TransferStatus("XMODEM", err.Error())
		code = 1
	}
	if onDone != nil {
		onDone(success, code)
	}
}

// ZmodemTransfer reads the requested file and pushes it via ZMODEM.
func ZmodemTransfer(ctx *Context, _ *Config, onDone OnDone) {
	data, name, err := transferPrelude(ctx, "ZMODEM", onDone)
	if err != nil {
		return
	}
	_ = ctx.Writeln("Please start your ZMODEM receiver NOW.")

	// Flush pause so old terminals' host monitors don't swallow the MD5
	// line along with the ZRQINIT trigger pattern.
	time.Sleep(500 * time.Millisecond)

	err = zmodem.SendBuffer(ctx.Conn, data, name)
	time.Sleep(500 * time.Millisecond)

	if err == nil {
		logger.TransferStatus("ZMODEM", "Transfer completed successfully")
		if onDone != nil {
			onDone(true, 0)
		}
		return
	}
	switch {
	case errors.Is(err, zmodem.ErrCancelled):
		_ = ctx.Writeln("")
		_ = ctx.Writeln("Transfer cancelled.")
		logger.TransferStatus("ZMODEM", "Transfer cancelled by user")
	case errors.Is(err, zmodem.ErrSkipped):
		_ = ctx.Writeln("")
		_ = ctx.Writeln("Transfer skipped.")
		logger.TransferStatus("ZMODEM", "Transfer skipped by receiver")
	default:
		logger.TransferStatus("ZMODEM", err.Error())
	}
	if onDone != nil {
		onDone(false, 1)
	}
}

// KermitTransfer reads the requested file and pushes it via classic Kermit.
func KermitTransfer(ctx *Context, _ *Config, onDone OnDone) {
	data, name, err := transferPrelude(ctx, "KERMIT", onDone)
	if err != nil {
		return
	}
	_ = ctx.Writeln("Please start your Kermit receiver NOW (classic short packets, no windowing).")

	time.Sleep(500 * time.Millisecond)

	startedAt := time.Now()
	lastLoggedAt := startedAt

	cfg := kermit.Config{
		ReadTimeout: 10 * time.Second,
		OnStart: func() {
			logger.TransferStatus("KERMIT", fmt.Sprintf("Transfer started for %s", ctx.RequestedFile))
		},
		OnStatus: func(evt kermit.StatusEvent) {
			now := time.Now()
			if now.Sub(lastLoggedAt) < 5*time.Second {
				return
			}
			elapsed := now.Sub(startedAt).Seconds()
			if elapsed <= 0 {
				return
			}
			bps := int(float64(evt.BytesSent) / elapsed)
			pct := 0
			if evt.TotalBytes > 0 {
				pct = evt.BytesSent * 100 / evt.TotalBytes
			}
			logger.TransferStatus("KERMIT",
				fmt.Sprintf("Transferred %d/%d bytes (%d%%) - %dB/sec",
					evt.BytesSent, evt.TotalBytes, pct, bps))
			lastLoggedAt = now
		},
	}

	err = kermit.Send(connAdapter{ctx.Conn}, data, name, cfg)
	if err != nil {
		logger.TransferStatus("KERMIT", err.Error())
		// Surface the error on the client terminal too — by the time the
		// sender saw an E packet the peer has usually exited its Kermit mode,
		// so these bytes land in the host terminal (minicom / screen / etc.).
		_ = ctx.Writeln("")
		_ = ctx.Writeln(fmt.Sprintf("Transfer failed: %v", err))
		if strings.Contains(err.Error(), "Write access denied") {
			_ = ctx.Writeln("Hint: some Kermit builds (incl. C-Kermit 9.0.302 on macOS) report")
			_ = ctx.Writeln("      \"Write access denied\" when the target file doesn't yet exist.")
			_ = ctx.Writeln("      Workaround: `touch` an empty file with the expected uppercase name")
			_ = ctx.Writeln("      in your receiver's download directory before starting the transfer.")
		}
		if onDone != nil {
			onDone(false, 1)
		}
		return
	}
	logger.TransferStatus("KERMIT", "Transfer completed successfully")
	if onDone != nil {
		onDone(true, 0)
	}
}

// connAdapter satisfies the xmodem package's deadlineConn interface using a
// plain net.Conn. (xmodem deliberately doesn't import net to stay testable
// with custom backing readers.)
type connAdapter struct {
	net.Conn
}

func (c connAdapter) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(t) }

func humanBytes(n int) string {
	return fmt.Sprintf("%d bytes", n)
}
