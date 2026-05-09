package session

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/arttu76/xfer/internal/kermit"
	"github.com/arttu76/xfer/internal/logger"
	"github.com/arttu76/xfer/internal/xmodem"
	"github.com/arttu76/xfer/internal/zmodem"
)

// Note: we can't import navigator or protocol here because they import
// session — the per-connection state machine below is therefore invoked
// *from* those packages via HandleConnection in cmd/xfer/main.go.

// OnDone is the completion callback for every transfer. exitCode follows
// the XMODEM convention (0 = success, non-zero = failure); for ZMODEM
// and Kermit we use 0/1 since they don't have a protocol-level code.
type OnDone func(success bool, exitCode int)

// transferPrelude flips the connection into transfer mode and returns the
// bytes + display name to send. The size / MD5 / "Ready to download" banner
// has already been printed by the caller (navigator on local pick, main on
// URL download) before the protocol prompt, so this prelude only emits a
// short "Initiating …" line and leaves the per-protocol "please start your
// receiver NOW" detail to the transfer function itself.
func transferPrelude(ctx *Context, protoName string, onDone OnDone) ([]byte, string, error) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")

	if ctx.RequestedBody == nil {
		_ = ctx.Writeln("Error: No file selected for transfer")
		if onDone != nil {
			onDone(false, -1)
		}
		return nil, "", errors.New("no file buffered for transfer")
	}

	_ = ctx.Writeln(fmt.Sprintf("Initiating %s transfer for %s", protoName, ctx.RequestedFile))
	return ctx.RequestedBody, ctx.RequestedName, nil
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
			// Reset so bps reflects actual transfer time, not the wait
			// for the user to start their receiver.
			startedAt = time.Now()
			lastLoggedAt = startedAt
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
			startedAt = time.Now()
			lastLoggedAt = startedAt
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

// OnReceive is the completion callback for upload (receive) handlers. The
// body is non-nil only on success; on failure errMsg describes what went
// wrong. Mirrors OnDone for the download direction but with payload + name
// so the navigator can persist the file before announcing completion.
type OnReceive func(success bool, name string, body []byte, errMsg string)

// XmodemReceive runs the XMODEM receiver. Caller (main loop) has paused the
// input-reader goroutine for the duration so we own conn.Read.
func XmodemReceive(ctx *Context, _ *Config, name string, onDone OnReceive) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")
	_ = ctx.Writeln(fmt.Sprintf("Initiating XMODEM upload to %s", name))
	_ = ctx.Writeln("Please start your XMODEM sender NOW.")

	startedAt := time.Now()
	lastLoggedAt := startedAt

	cfg := xmodem.Config{
		ReadTimeout: 10 * time.Second,
		OnStart: func() {
			startedAt = time.Now()
			lastLoggedAt = startedAt
			logger.TransferStatus("XMODEM-RX", fmt.Sprintf("Receive started for %s", name))
		},
		OnStatus: func(evt xmodem.StatusEvent) {
			now := time.Now()
			if now.Sub(lastLoggedAt) < 5*time.Second {
				return
			}
			elapsed := now.Sub(startedAt)
			bytes := evt.Block * xmodem.BlockSize
			bps := 0
			if secs := elapsed.Seconds(); secs > 0 {
				bps = int(float64(bytes) / secs)
			}
			logger.TransferStatus("XMODEM-RX",
				fmt.Sprintf("Received block %d (%d bytes, %dB/sec)", evt.Block, bytes, bps))
			lastLoggedAt = now
		},
	}

	body, err := xmodem.Receive(connAdapter{ctx.Conn}, cfg)
	if err != nil {
		logger.TransferStatus("XMODEM-RX", err.Error())
		if onDone != nil {
			onDone(false, name, nil, err.Error())
		}
		return
	}
	logger.TransferStatus("XMODEM-RX", fmt.Sprintf("Receive complete: %s (%d bytes)", name, len(body)))
	if onDone != nil {
		onDone(true, name, body, "")
	}
}

// ZmodemReceive runs the ZMODEM receiver. The destination filename is
// carried in the ZFILE frame, so unlike the XMODEM path we don't ask the
// user to type one upfront.
func ZmodemReceive(ctx *Context, _ *Config, onDone OnReceive) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")
	_ = ctx.Writeln("Initiating ZMODEM upload")
	_ = ctx.Writeln("Please start your ZMODEM sender NOW.")

	startedAt := time.Now()
	lastLoggedAt := startedAt

	cfg := zmodem.ReceiveConfig{
		OnStart: func() {
			startedAt = time.Now()
			lastLoggedAt = startedAt
			logger.TransferStatus("ZMODEM-RX", "Receive started")
		},
		OnStatus: func(evt zmodem.ReceiveStatus) {
			now := time.Now()
			if now.Sub(lastLoggedAt) < 5*time.Second {
				return
			}
			elapsed := now.Sub(startedAt).Seconds()
			bps := 0
			if elapsed > 0 {
				bps = int(float64(evt.Bytes) / elapsed)
			}
			pct := 0
			if evt.Total > 0 {
				pct = evt.Bytes * 100 / evt.Total
			}
			logger.TransferStatus("ZMODEM-RX",
				fmt.Sprintf("Received %d/%d bytes (%d%%) - %dB/sec",
					evt.Bytes, evt.Total, pct, bps))
			lastLoggedAt = now
		},
	}

	res, err := zmodem.Receive(ctx.Conn, cfg)
	if err != nil {
		logger.TransferStatus("ZMODEM-RX", err.Error())
		if onDone != nil {
			onDone(false, "", nil, err.Error())
		}
		return
	}
	logger.TransferStatus("ZMODEM-RX",
		fmt.Sprintf("Receive complete: %s (%d bytes)", res.Filename, len(res.Data)))
	if onDone != nil {
		onDone(true, res.Filename, res.Data, "")
	}
}

// KermitReceive runs the Kermit receiver. The filename comes in over the
// F packet — we surface it to the caller via OnReceive.
func KermitReceive(ctx *Context, _ *Config, onDone OnReceive) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")
	_ = ctx.Writeln("Initiating Kermit upload")
	_ = ctx.Writeln("Please start your Kermit sender NOW.")

	startedAt := time.Now()
	lastLoggedAt := startedAt

	cfg := kermit.ReceiveConfig{
		ReadTimeout: 30 * time.Second,
		OnStart: func() {
			startedAt = time.Now()
			lastLoggedAt = startedAt
			logger.TransferStatus("KERMIT-RX", "Receive started")
		},
		OnStatus: func(evt kermit.StatusEvent) {
			now := time.Now()
			if now.Sub(lastLoggedAt) < 5*time.Second {
				return
			}
			elapsed := now.Sub(startedAt).Seconds()
			bps := 0
			if elapsed > 0 {
				bps = int(float64(evt.BytesSent) / elapsed)
			}
			pct := 0
			if evt.TotalBytes > 0 {
				pct = evt.BytesSent * 100 / evt.TotalBytes
			}
			logger.TransferStatus("KERMIT-RX",
				fmt.Sprintf("Received %d/%d bytes (%d%%) - %dB/sec",
					evt.BytesSent, evt.TotalBytes, pct, bps))
			lastLoggedAt = now
		},
	}

	res, err := kermit.Receive(connAdapter{ctx.Conn}, cfg)
	if err != nil {
		logger.TransferStatus("KERMIT-RX", err.Error())
		if onDone != nil {
			onDone(false, "", nil, err.Error())
		}
		return
	}
	logger.TransferStatus("KERMIT-RX",
		fmt.Sprintf("Receive complete: %s (%d bytes)", res.Filename, len(res.Data)))
	if onDone != nil {
		onDone(true, res.Filename, res.Data, "")
	}
}

// connAdapter satisfies the xmodem package's deadlineConn interface using a
// plain net.Conn. (xmodem deliberately doesn't import net to stay testable
// with custom backing readers.)
type connAdapter struct {
	net.Conn
}

func (c connAdapter) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(t) }
