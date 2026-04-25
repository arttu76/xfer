# XFER - File Transfer Server for Old Computers

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
- Auto-detects the connected terminal's size on connect (ANSI cursor-position probe) so the viewer and directory listing lay out correctly without manual configuration; falls back gracefully on terminals that don't answer
- Paginated directory browsing with `[M]ore` / `[S]earch`: large directories don't scroll off the screen, and the search filter narrows a long listing to just the files whose names match a substring (case-insensitive)
- Download a file directly from a URL: the server fetches it (http/https) straight into memory and streams it to the old computer, no scratch file on disk
- Paste long URLs into the server's own keyboard instead of typing them on the old terminal; both sides can type, first Enter wins (opt out with `--no-stdin-url`)
- Tuned for the old terminal programs of the era (CRC16, 1 KB subpackets, 8 KB
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
xfer v1.2.2 — XMODEM / ZMODEM / Kermit file server + viewer for old computers

Usage: xfer [flags]

  -p, --port <number>       port to use (default: 23)
  -d, --directory <string>  directory to serve (default: current directory)
  -s, --secure              secure mode: don't allow user to change directories
  -n, --no-url              disallow the [U]RL download option in the file listing
  -c, --no-stdin-url        do not inject stdin lines into a client's URL prompt
  -w, --wirelog <path>      hexdump every wire byte to file ("-" for stderr)
      --term-width <n>      default/fallback terminal width (default: 40)
      --term-height <n>     default/fallback terminal height (default: 20)
      --no-term-detect      skip terminal-size auto-detection on connect
      --term-detect-timeout <ms> how long to wait for the probe reply (default: 2000)
  -V, --version             print version and exit
  -h, --help                print this help and exit
```

Note: the default port is 23 (telnet), which on most systems requires
administrator/root privileges to bind. You'll most likely want to pick a
higher port instead, for example 2000:

```
$ xfer -p 2000
2026-04-22T12:15:30.123Z Server now listening on 192.168.1.194:2000 / 10.0.0.5:2000
```

### 2. On your old computer, use terminal to connect:

We're using the Hayes AT command to "dial" into the host computer's IP and port:

```
ATDT192.168.1.194:2000
Detecting terminal size...
Terminal size: 80x25
----- /Users/arttu/games -----
1 <D> ..
2 ... paradroid.prg
3 ... mule.prg
4 ... wizball.prg
1-4, [U]RL, [S]earch, [R]efresh, e[X]it: 3
Ready to download mule.prg
Size: 48829 bytes
MD5:  9a982e21160b982a02fd43412f14e127
/Users/arttu/games/mule.prg - [X]MODEM, [Z]MODEM, [K]ermit, [V]iew, or [C]ancel?: Z
Initiating ZMODEM transfer for /Users/arttu/games/mule.prg
Please start your ZMODEM receiver NOW.
```

Size and MD5 are shown **before** the protocol prompt so you can see
how big the file is before picking a protocol — XMODEM is fine for
small files, ZMODEM is much faster on larger ones.

For ZMODEM, most terminals (Term 4.8, NComm, SyncTerm, etc.) auto-detect
and start receiving. For XMODEM and Kermit you need to manually trigger
the receive in your terminal program.

You can also browse the host computer's file system (unless you start the xfer with "secure mode" which allows you to only browse files and not to move to another directory)

### Terminal size auto-detection

On connect xfer sends a standard ANSI cursor-position probe (`ESC[6n`)
and uses the terminal's reply to pick a size for the directory listing
and the file viewer. Most modern terminal emulators answer in a few
milliseconds; vintage terminals over a wifi modem (Term 4.8 on Amiga,
for example) take ~1 second. xfer waits up to two seconds and then
either:

- prints `Terminal size: 80x25` (whatever the terminal reported), or
- prints `Terminal size not detected, using 40x20` and falls back to the
  configured defaults.

On a terminal that doesn't understand CSI escape sequences (e.g. a plain
PETSCII terminal on a C64), the probe bytes echo as a few literal
characters on screen — the leading `Detecting terminal size...` line is
there to frame those characters as detection noise rather than
unexplained garbage.

Override the defaults with `--term-width` / `--term-height`, skip the
probe entirely with `--no-term-detect` if you're connecting from a
terminal that mis-handles the escape sequence, or extend the wait with
`--term-detect-timeout 5000` (milliseconds) if your link is even slower
than two seconds.

### Listing big directories

When a directory has more files than fit on one screen the listing
pauses with `[M]ore, [S]earch:` after each page. Press `M` to keep
paging, or `S` to filter — type a substring and the listing redraws as
just the files whose names match (case-insensitive), preserving the
original entry numbers so the digit you type at the menu still picks
the right file. From the final menu prompt `S` triggers the same
search; pressing Enter on an empty term takes you back to the unfiltered
listing.

### Downloading from a URL

You don't have to pre-stage a file on the host's disk. Press **U** at the
listing to type a URL; the server fetches it over http/https, shows the
size and MD5, and hands off to your pick of XMODEM / ZMODEM / Kermit (or
the viewer). The body is kept in memory — nothing is written to the
host's disk. Submitting an empty URL takes you back to the listing; a
failed fetch (bad host, 404, etc.) re-prompts for the URL so you can
correct a typo.

Disable this feature with `-n` / `--no-url` (see Security Notes below).

**Type the URL on whichever keyboard is convenient.** Long URLs are
miserable to type on a retro keyboard, so once the URL prompt is up you
can enter the URL on *either* side — the old computer's terminal or
directly into xfer's console on the modern computer (paste or type, then
press Enter). Whichever side hits Enter first wins, and the characters
echo on the old computer's screen as if typed there.

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
| `q` / `c` | quit back to the file list                   |
| `h`       | show help                                    |

The viewer's terminal dimensions come from the connect-time auto-detect
(see "Terminal size auto-detection" above) — there's no in-viewer
override because nothing on the wire would carry it back if the user
overrode it for a URL-fetched body. Pass `--term-width` / `--term-height`
on the server command line if you need to force a specific size.

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
- `internal/urlfetch/` — http/https downloader used by the `U=url` option
- `internal/urlconsole/` — registry + stdin reader for server-side URL paste
- `internal/xmodem/` — XMODEM sender (CRC-16 + checksum, NAK retransmit, EOT)
- `internal/zmodem/` — ZMODEM sender tuned for old-terminal compatibility
  (CRC-16 only, 1 KB subpackets, 8 KB frames, ESCCTL negotiation, lrzsz
  fileinfo, `rz\r` trigger, 5×CAN cancel)
- `internal/kermit/` — Kermit sender: long packets, type-1/2/3 block checks
  (CRC-16-Kermit), 8-bit quoting, run-length encoding, and sliding-window
  flow control — feature set negotiated from the receiver's S-ACK
- `internal/logger/` — timestamped stderr logging
- `internal/testutil/` — shared loopback / capture / golden-diff helpers for tests
- `internal/constants/` — CLI defaults and menu prefixes

Wire-format golden fixtures sit next to the package that loads them —
`internal/xmodem/testdata/` and `internal/zmodem/testdata/`. The `go test`
runner treats any `testdata/` directory as a first-class test-only asset
(skipped by `go build`, `go vet`, and module tidying), so this is the
idiomatic layout for committed fixtures in Go.

### Building

```
$ make build            # local binary in ./bin/xfer
$ make dist             # cross-compile for linux/macos/windows × amd64/arm64
$ make test             # run the test suite
```

### Tests

The XMODEM and ZMODEM packages each include a byte-level wire-format test
that compares the sender's output against a committed golden dump in
the package's own `testdata/` directory. The goldens were captured from
a known-good session that had been tested against real old terminals
(Term 4.8 on Amiga, lrzsz on Linux, SyncTerm, NComm), so passing them
proves wire-format compatibility with those receivers byte-for-byte.

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
- The URL download feature (`U` at the listing) makes the *server* perform
  the HTTP request on its own network. A connected client can therefore ask
  xfer to fetch any URL the host itself can reach — including machines on
  the host's private LAN that the client couldn't normally see. If xfer
  runs on a network where that matters, disable the feature with `-n` /
  `--no-url`. Downloads are capped at 64 MB and carry a 30-second timeout.

## License

This project is open source and available under the WTFPL.
