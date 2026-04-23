# XFER - File Transfer Server for Retro Computers

## Overview

Getting files *onto* an old computer is the hard part — floppies rot, serial cables are fiddly, and most vintage machines can't talk to modern networks on their own. A wifi modem (such as the [WiModem232 Pro](https://www.cbmstuff.com/index.php?route=product/product&product_id=113)) solves the networking side, but the old machine still needs something on the other end of the connection to serve the files.

XFER is that something. Run it on your modern computer, then from the old machine's terminal program "dial" into it over the wifi modem: browse the modern computer's file system, view files inline, and download any of them to the old side using XMODEM, ZMODEM, or Kermit.

## Features

- Easy: download one file and run it
- Allows browsing of the host file system
- File transfer using XMODEM protocol (very slow but maximally compatible)
- File transfer using ZMODEM protocol (faster; built in, no extra tools needed)
- File transfer using classic Kermit protocol (for clients that only have Kermit — e.g. some CP/M and mainframe terminals)
- Built-in file viewer: inspect files on the host without downloading first (text or hex dump, scroll, search, adjustable terminal size)
- Tuned for retro terminals on old computers (CRC16, 1 KB subpackets, 8 KB
  frames, lrzsz-style ZFILE metadata, ESCCTL negotiation, CAN-burst cancel)
- Shows an MD5 of the file before the transfer so you can verify integrity
- Secure mode to restrict directory access
- Binaries available for Windows, macOS and Linux

## How to get it

[Download the suitable executable for your operating system from the releases page](https://github.com/arttu76/xfer/releases) and save it on your hard drive.

## Usage

### 1. Start XFER on your "modern" computer:

When running xfer, you probably don't need to change any options, but you can use the -h command line switch to see what options are available:

```
$ xfer -h
xfer v1.1.0 — XMODEM / ZMODEM / Kermit file server + viewer for retro computers

Usage: xfer [flags]

  -p, --port <number>       port to use (default: 23)
  -d, --directory <string>  directory to serve (default: current directory)
  -s, --secure              secure mode: don't allow user to change directories
  -V, --version             print version and exit
  -h, --help                print this help and exit
```

Note: the default port is 23 (telnet), which on most systems requires
administrator/root privileges to bind. You'll most likely want to pick a
higher port instead, for example 2000:

```
$ xfer -p 2000
2026-04-22T12:15:30.123Z [INFO] Server now listening on 192.168.1.194:2000 / 10.0.0.5:2000
```

### 2. On your old computer, use terminal to connect:

We're using the Hayes AT command to "dial" into the host computer's IP and port:

```
ATDT192.168.1.194:2000
----- /Users/arttu/games -----
1 <D> ..
2 ... paradroid.prg
3 ... mule.prg
4 ... wizball.prg
Enter 1-4, R=refresh, X=exit: 3

/Users/arttu/games/mule.prg — [X]MODEM, [Z]MODEM, [K]ermit, [V]iew, or [C]ancel?: Z
Ready to download mule.prg
MD5: 9a982e21160b982a02fd43412f14e127
Initiating ZMODEM transfer for /Users/arttu/games/mule.prg
Please start your ZMODEM receiver NOW.
```

For ZMODEM, most terminals (Term 4.8, NComm, SyncTerm, etc.) auto-detect
and start receiving. For XMODEM you need to manually trigger the receive
in your terminal program.

You can also browse the host computer's file system (unless you start the xfer with "secure mode" which allows you to only browse files and not to move to another directory)

### File viewer

Instead of downloading a file, pick **V** at the transfer prompt to view
it inline. The viewer auto-detects text vs binary and picks char or hex
display accordingly. Single-keystroke controls (no arrow keys needed):

| key       | action                                       |
|-----------|----------------------------------------------|
| `f` / `b` | scroll one line forward / back               |
| `d` / `u` | scroll one page down / up (SPACE = `d`)      |
| `m`       | toggle hex / char display                    |
| `s`       | search; empty input repeats the last search  |
| `l`       | set terminal width (default 40) and height (default 20) so the viewer lays out correctly on your terminal |
| `q` / `c` | quit back to the file list                   |
| `?`       | show help                                    |

### 3. That's it!

Enjoy!

## Running from source

Don't want to download binaries? If you have Go installed, just do:

```
$ git clone https://github.com/arttu76/xfer
$ cd xfer
$ go run ./cmd/xfer
```

## Development

The project is written in Go and uses a modular architecture:

- `cmd/xfer/` — CLI entry point, flag parsing, TCP accept loop
- `internal/session/` — per-connection state machine and transfer handlers
- `internal/navigator/` — file browsing, listing, secure-mode path checks
- `internal/protocol/` — XMODEM/ZMODEM/Kermit/View/cancel selection prompt
- `internal/viewer/` — inline text/hex file viewer (scroll, search, resize)
- `internal/xmodem/` — XMODEM sender (CRC-16 + checksum, NAK retransmit, EOT)
- `internal/zmodem/` — ZMODEM sender tuned for retro-terminal compatibility
  (CRC-16 only, 1 KB subpackets, 8 KB frames, ESCCTL negotiation, lrzsz
  fileinfo, `rz\r` trigger, 5×CAN cancel)
- `internal/kermit/` — Kermit sender: long packets, type-1/2/3 block checks
  (CRC-16-Kermit), 8-bit quoting, run-length encoding, and sliding-window
  flow control — feature set negotiated from the receiver's S-ACK
- `internal/logger/` — timestamped stderr logging
- `internal/testutil/` — shared loopback / capture / golden-diff helpers for tests
- `internal/constants/` — CLI defaults and menu prefixes
- `test/golden/` — committed byte-exact wire-format fixtures

### Building

```
$ make build            # local binary in ./bin/xfer
$ make dist             # cross-compile for linux/macos/windows × amd64/arm64
$ make test             # run the test suite
```

### Tests

The XMODEM and ZMODEM packages each include a byte-level wire-format test
that compares the sender's output against a committed golden dump in
`test/golden/`. The goldens were captured from a known-good session that
had been tested against real retro terminals (Term 4.8 on Amiga, lrzsz on
Linux, SyncTerm, NComm), so passing them proves wire-format compatibility
with those receivers byte-for-byte.

The rest of the suite covers the non-golden behaviors: ZRPOS resume,
cancel (5×CAN → 8×CAN + 10×BS echo), activity timeout, subpacket sizing,
8-per-ACK pacing, ESCCTL negotiation for lrzsz / Term 4.8 / NComm caps,
lrzsz fileinfo format, XMODEM CRC / checksum modes and block wrap past
32 KB, navigator path-traversal guard, and protocol selector branches.

## Security Notes

- The server validates file paths to prevent directory traversal attacks
- Use the `-s` (secure) flag to restrict users to the initial directory
- All transfers and the viewer read the file into memory and stream from
  the buffer — no temporary files are written to disk

## License

This project is open source and available under the WTFPL.
