package logger

import (
	"fmt"
	"os"
	"time"
)

// Emits lines as "2026-04-22T13:00:00.000Z message". Severity prefixes
// were dropped — the few classes of events on stderr don't carry enough
// signal to be worth filtering on, and deeper diagnosis is done with
// --wirelog rather than by grepping log levels.

func Info(msg string)  { emit(msg) }
func Error(msg string) { emit(msg) }
func Debug(msg string) { emit(msg) }

// TransferStatus logs a protocol-tagged status line.
func TransferStatus(protocol, msg string) {
	emit(fmt.Sprintf("%s: %s", protocol, msg))
}

func emit(msg string) {
	fmt.Fprintf(os.Stderr, "%s %s\n",
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)
}
