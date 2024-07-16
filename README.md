# XFER - XMODEM File Transfer Utility

## Overview

If you have a wifi modem for old computer (such as [WiModem232 Pro](https://www.cbmstuff.com/index.php?route=product/product&product_id=113)), you might need a software to be run on a modern "host" computer that allows the old computer browse and download files from the "host" computer.

XFER is such a program: run it on your computer, connect from your retro computer's terminal program and download any files you like.

## Features

- Easy: download one file and run it
- Allows browsing of the host file system
- File transfer using XMODEM protocol (very slow but also very compatible)
- Cross-platform support (Windows, macOS, Linux)

## How to get it

[Download the suitable executable for your operating system from the releases page](https://github.com/arttu76/xfer/releases) and save it on your hard drive. When running xfer, you probably don't need to change any options, but you can use the -h command line switch to see what options are available:

```
$ bin/osx/xfer -h
Usage: xfer [options]

Start Xmodem on your computer to allow old computers to download files with wifi modem devices.

Options:
  -V, --version             output the version number
  -p, --port <number>       port to use (default: 23)
  -d, --directory <string>  directory to serve
  -s, --secure              secure mode: don't allow user to change directories
  -h, --help                display help for command
```

## Usage

### 1. Start xfer on your "modern" computer:

```
$ bin/osx/xfer
Tue Jul 16 2024 11:51:10 GMT+0300 Server now listening on 192.168.1.194:23
```

### 2. On your "retro" computer, use terminal to connect:

```
ATDT192.168.1.194:23
----- /Users/arttu/games -----
1 <D> ..
2 ... paradroid.prg
3 ... mule.prg
4 ... wizball.prg
Enter 1-4, R=refresh, X=exit: 3

Start transferring /Users/arttu/games/mule.prg via XMODEM? [Y/n]: Yes

Initiating XMODEM transfer for /Users/arttu/games/mule.prg
Please start your XMODEM receiver NOW.
```

... and start the download on your terminal program. Alternatively you can browse the host computer's directories (unless you start the xfer with "secure mode" which allows you to only browser the directory from which the xfer was started from)

### 3. That's it!
