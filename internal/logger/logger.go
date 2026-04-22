package logger

import (
	"fmt"
	"os"
	"time"
)

// Emits lines as "2026-04-22T13:00:00.000Z [LEVEL] message".

func Info(msg string)  { emit("INFO", msg) }
func Error(msg string) { emit("ERROR", msg) }
func Debug(msg string) { emit("DEBUG", msg) }

// TransferStatus logs a protocol-tagged status line.
func TransferStatus(protocol, msg string) {
	Info(fmt.Sprintf("%s: %s", protocol, msg))
}

func emit(level, msg string) {
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), level, msg)
}
