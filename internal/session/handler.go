package session

import (
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/xmodem"
	"github.com/solvalou/xfer/internal/zmodem"
)

// Note: we can't import navigator or protocol here because they import
// session — the per-connection state machine below is therefore invoked
// *from* those packages via HandleConnection in cmd/xfer/main.go.

// XmodemTransfer reads the requested file and pushes it to the client via
// XMODEM. Emits structured progress log lines every few seconds.
func XmodemTransfer(ctx *Context, _ *Config, onDone func(success bool, exitCode int)) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")

	if ctx.RequestedFile == "" {
		_ = ctx.Writeln("Error: No file selected for transfer")
		if onDone != nil {
			onDone(false, -1)
		}
		return
	}

	data, err := os.ReadFile(ctx.RequestedFile)
	if err != nil {
		logger.TransferStatus("XMODEM", fmt.Sprintf("Error reading file: %v", err))
		_ = ctx.Writeln(fmt.Sprintf("Error reading file: %v", err))
		if onDone != nil {
			onDone(false, -1)
		}
		return
	}

	name := filepath.Base(ctx.RequestedFile)
	blocks := (len(data) + 127) / 128
	if blocks == 0 {
		blocks = 1
	}
	_ = ctx.Writeln(fmt.Sprintf("Ready to download %s", name))
	_ = ctx.Writeln(fmt.Sprintf("MD5: %x", md5.Sum(data)))
	_ = ctx.Writeln(fmt.Sprintf("Initiating XMODEM transfer for %s", ctx.RequestedFile))
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
func ZmodemTransfer(ctx *Context, _ *Config, onDone func(success bool)) {
	ctx.Mode = ModeTransferFile
	_ = ctx.Writeln("")

	if ctx.RequestedFile == "" {
		_ = ctx.Writeln("Error: No file selected for transfer")
		if onDone != nil {
			onDone(false)
		}
		return
	}
	data, err := os.ReadFile(ctx.RequestedFile)
	if err != nil {
		logger.TransferStatus("ZMODEM", fmt.Sprintf("Error reading file: %v", err))
		_ = ctx.Writeln(fmt.Sprintf("Error reading file: %v", err))
		if onDone != nil {
			onDone(false)
		}
		return
	}
	name := filepath.Base(ctx.RequestedFile)
	_ = ctx.Writeln(fmt.Sprintf("Ready to download %s", name))
	_ = ctx.Writeln(fmt.Sprintf("MD5: %x", md5.Sum(data)))
	_ = ctx.Writeln(fmt.Sprintf("Initiating ZMODEM transfer for %s", ctx.RequestedFile))
	_ = ctx.Writeln("Please start your ZMODEM receiver NOW.")

	// Flush pause so retro terminals' host monitors don't swallow the MD5
	// line along with the ZRQINIT trigger pattern.
	time.Sleep(500 * time.Millisecond)

	err = zmodem.SendBuffer(ctx.Conn, data, name)
	time.Sleep(500 * time.Millisecond)

	if err == nil {
		logger.TransferStatus("ZMODEM", "Transfer completed successfully")
		if onDone != nil {
			onDone(true)
		}
		return
	}
	if errors.Is(err, zmodem.ErrCancelled) {
		_ = ctx.Writeln("")
		_ = ctx.Writeln("Transfer cancelled.")
		logger.TransferStatus("ZMODEM", "Transfer cancelled by user")
	} else {
		logger.TransferStatus("ZMODEM", err.Error())
	}
	if onDone != nil {
		onDone(false)
	}
}

// connAdapter satisfies the xmodem package's deadlineConn interface using a
// plain net.Conn. (xmodem deliberately doesn't import net to stay testable
// with custom backing readers.)
type connAdapter struct {
	net.Conn
}

func (c connAdapter) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(t) }
