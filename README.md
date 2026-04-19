# XFER - File Transfer Server for Retro Computers

## Overview

If you have a wifi modem for an old computer (such as [WiModem232 Pro](https://www.cbmstuff.com/index.php?route=product/product&product_id=113)), you might need a software to be run on a modern "host" computer that allows the old computer browse and download files from the "host" computer.

XFER is such a program: run it on your computer, connect from your retro computer's terminal program and download any files you like.

## Features

- Easy: download one file and run it
- Allows browsing of the host file system
- File transfer using XMODEM protocol (very slow but maximally compatible)
- File transfer using ZMODEM protocol (faster; built in, no extra tools needed)
- Tuned for Amiga Term 4.8 / xprzmodem.library compatibility (CRC16, 1 KB
  subpackets, 8 KB frames, lrzsz-style ZFILE metadata, ESCCTL negotiation,
  CAN-burst cancel)
- Shows an MD5 of the file before the transfer so you can verify integrity
- Case-insensitive file sorting for better navigation
- Secure mode to restrict directory access
- Binaries available for Windows, macOS and Linux

## How to get it

[Download the suitable executable for your operating system from the releases page](https://github.com/arttu76/xfer/releases) and save it on your hard drive.

## Usage

### 1. Start XFER on your "modern" computer:

When running xfer, you probably don't need to change any options, but you can use the -h command line switch to see what options are available:

```
$ xfer -h
Usage: xfer [options]

Start XFER on your computer to allow (retro?) computers to download files with devices like WiModem232.

Options:
  -V, --version             output the version number
  -p, --port <number>       port to use (default: 23)
  -d, --directory <string>  directory to serve (default: current directory)
  -s, --secure              secure mode: don't allow user to change directories
  -h, --help                display help for command
```

...but most of the time you can just start it without any options:

```
$ xfer
2024-07-18T12:15:30.123Z [INFO] Server now listening in 192.168.1.194:23
```

ZMODEM support is built in — no external tools required.

### 2. On your "retro" computer, use terminal to connect:

We're using the Hayes AT command to "dial" into the host computer's IP and port:

```
ATDT192.168.1.194:23
----- /Users/arttu/games -----
1 <D> ..
2 ... paradroid.prg
3 ... mule.prg
4 ... wizball.prg
Enter 1-4, R=refresh, X=exit: 3

Transfer /Users/arttu/games/mule.prg via [X]MODEM, [Z]MODEM, or [C]ancel?: Z
Ready to download mule.prg
MD5: 9a982e21160b982a02fd43412f14e127
Initiating ZMODEM transfer for /Users/arttu/games/mule.prg
Please start your ZMODEM receiver NOW.
```

For ZMODEM, most terminals (Term 4.8, NComm, SyncTerm, etc.) auto-detect
and start receiving. For XMODEM you need to manually trigger the receive
in your terminal program.

You can also browse the host computer's file system (unless you start the xfer with "secure mode" which allows you to only browse files and not to move to another directory)

### 3. That's it!

Enjoy!

## Running from source

Don't want to download binaries? If you have development tools on your computer, just do:

```
$ git clone https://github.com/arttu76/xfer
$ cd xfer
$ npm install
$ npm run start
```

## Development

The project is written in TypeScript and uses a modular architecture:

- `server.ts` - Main server implementation
- `fileNavigator.ts` - File browsing and navigation functionality
- `protocolSelector.ts` - Protocol selection and common transfer logic
- `xmodemTransfer.ts` - XMODEM service-layer wrapper (uses `xmodem.ts` library)
- `zmodemTransfer.ts` - ZMODEM service-layer wrapper
- `zmodemEngine.ts` - In-process ZMODEM sender built on the `zmodem2` library,
  patched for retro-terminal compatibility (CRC16, 1 KB subpackets, 8 KB
  frames, ESCCTL negotiation, lrzsz-format ZFILE)
- `utils.ts` - Common utility functions (MD5, sleep, terminal-flush constant)
- `logger.ts` - Centralized logging system
- `constants.ts` - Configuration constants
- `types.ts` - TypeScript type definitions

### Building

To build the project:

```
$ npm run build
```

This will compile the TypeScript files to JavaScript in the `js/` directory.

## Security Notes

- The server validates file paths to prevent directory traversal attacks
- Use the `-s` (secure) flag to restrict users to the initial directory
- ZMODEM transfers stream directly from the in-memory file buffer — no
  temporary files are written to disk

## License

This project is open source and available under the MIT License.
