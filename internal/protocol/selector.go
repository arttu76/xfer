package protocol

import (
	"fmt"
	"strings"
	"time"

	"github.com/arttu76/xfer/internal/logger"
	"github.com/arttu76/xfer/internal/navigator"
	"github.com/arttu76/xfer/internal/session"
)

func firstByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

// ShowProtocolPrompt asks the user to choose XMODEM / ZMODEM / Kermit / View / cancel.
func ShowProtocolPrompt(ctx *session.Context) {
	_ = ctx.Write(fmt.Sprintf("%s - [X]MODEM, [Z]MODEM, [K]ermit, [V]iew, or [C]ancel?: ", ctx.RequestedFile))
}

// ShowTransferComplete prints the completion line and returns to file list.
// Pauses first so terminal emulators (minicom, c-kermit CONNECT) have time
// to return from their post-transfer dialog/state — otherwise the whole
// completion message + listing lands during the client's protocol→terminal
// transition and gets dropped or overdrawn, forcing the user to press R.
func ShowTransferComplete(ctx *session.Context, cfg *session.Config, proto string, success bool, exitCode int) {
	time.Sleep(750 * time.Millisecond)
	_ = ctx.Writeln("")
	_ = ctx.Writeln("")
	if success {
		msg := fmt.Sprintf("%s transfer of %s completed successfully", proto, ctx.RequestedFile)
		logger.Info(msg)
		_ = ctx.Writeln(msg)
	} else {
		msg := fmt.Sprintf("%s transfer stopped with exit code %d", proto, exitCode)
		logger.Info(msg)
		_ = ctx.Writeln(msg)
	}
	navigator.ListFiles(ctx, cfg)
}

// ShowUploadProtocolPrompt asks which protocol the client will use to send
// a file up to us. ZMODEM/Kermit carry the filename in the protocol;
// XMODEM doesn't, so picking XMODEM transitions to a filename-entry step.
func ShowUploadProtocolPrompt(ctx *session.Context) {
	_ = ctx.Write("Upload via [X]MODEM, [Z]MODEM, [K]ermit, or [C]ancel?: ")
}

// DispatchUploadProtocol routes the user's choice for an upload. XMODEM
// transitions to the filename-entry mode (the protocol carries no name);
// ZMODEM and Kermit jump straight to their receivers and pull the
// destination name out of the wire format.
func DispatchUploadProtocol(
	ctx *session.Context,
	input string,
	cfg *session.Config,
	startZ func(*session.Context),
	startK func(*session.Context),
) {
	switch firstByte(strings.ToLower(strings.TrimSpace(input))) {
	case 0:
		_ = ctx.Writeln("")
		ShowUploadProtocolPrompt(ctx)
	case 'x':
		_ = ctx.Writeln("XMODEM")
		ctx.Mode = session.ModeEnterUploadName
		_ = ctx.Write("Enter destination filename (empty=cancel): ")
	case 'z':
		_ = ctx.Writeln("ZMODEM")
		if startZ != nil {
			startZ(ctx)
		}
	case 'k':
		_ = ctx.Writeln("KERMIT")
		if startK != nil {
			startK(ctx)
		}
	case 'c', 'n':
		_ = ctx.Writeln("Cancelled")
		navigator.ListFiles(ctx, cfg)
	default:
		_ = ctx.Writeln("Invalid option. Please enter X, Z, K, or C.")
		ShowUploadProtocolPrompt(ctx)
	}
}

// ShowUploadComplete prints the upload-side completion line and returns to
// the listing. Mirrors ShowTransferComplete but with upload-flavored
// wording so the operator can tell the directions apart in logs.
func ShowUploadComplete(ctx *session.Context, cfg *session.Config, proto, name string, success bool, errMsg string) {
	time.Sleep(750 * time.Millisecond)
	_ = ctx.Writeln("")
	_ = ctx.Writeln("")
	if success {
		msg := fmt.Sprintf("%s upload of %s completed successfully", proto, name)
		logger.Info(msg)
		_ = ctx.Writeln(msg)
	} else {
		msg := fmt.Sprintf("%s upload failed: %s", proto, errMsg)
		logger.Info(msg)
		_ = ctx.Writeln(msg)
	}
	navigator.ListFiles(ctx, cfg)
}

// ConfirmAndStartTransfer routes based on the first sanitized character of
// the user's reply: x → XMODEM, z → ZMODEM, k → Kermit, v → View,
// c/n → cancel, else re-prompt.
func ConfirmAndStartTransfer(
	ctx *session.Context,
	input string,
	cfg *session.Config,
	startX func(*session.Context),
	startZ func(*session.Context),
	startK func(*session.Context),
	startV func(*session.Context),
) {
	switch firstByte(strings.ToLower(strings.TrimSpace(input))) {
	case 0:
		_ = ctx.Writeln("")
		ShowProtocolPrompt(ctx)
	case 'x':
		_ = ctx.Writeln("XMODEM")
		if startX != nil {
			startX(ctx)
		}
	case 'z':
		_ = ctx.Writeln("ZMODEM")
		if startZ != nil {
			startZ(ctx)
		}
	case 'k':
		_ = ctx.Writeln("KERMIT")
		if startK != nil {
			startK(ctx)
		}
	case 'v':
		_ = ctx.Writeln("VIEW")
		if startV != nil {
			startV(ctx)
		}
	case 'c', 'n':
		_ = ctx.Writeln("Cancelled")
		navigator.ListFiles(ctx, cfg)
	default:
		_ = ctx.Writeln("Invalid option. Please enter X, Z, K, V, or C.")
		ShowProtocolPrompt(ctx)
	}
}
