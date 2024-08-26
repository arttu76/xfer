import * as net from 'net';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import { Xmodem } from 'xmodem.ts';
import { Command, InvalidArgumentError } from 'commander';

const program = new Command();

program
  .name('xfer')
  .description(
    'Start XFER on your computer to allow (retro?) computers to download files with devices like WiModem232.'
  )
  .version('1.0.3');

program
  .option(
    '-p, --port <number>',
    'port to use',
    (value: string): number => {
      const port = parseInt(value, 10);
      if (isNaN(port)) {
        throw new InvalidArgumentError('Not a number.');
      }
      if (port < 0 || port > 65535) {
        throw new InvalidArgumentError('Port must be between 0 and 65535.');
      }
      return port;
    },
    23
  )
  .option(
    '-d, --directory <string>',
    'directory to serve',
    (value: string): string => {
      const errorMessage = `${value} is not a valid directory.`;
      try {
        const resolvedPath = path.resolve(value);
        if (!fs.statSync(resolvedPath).isDirectory()) {
          throw new InvalidArgumentError(errorMessage);
        }
      } catch (error) {
        throw new InvalidArgumentError(errorMessage);
      }
      return value;
    },
    process.cwd()
  )
  .option('-s, --secure', "secure mode: don't allow user to change directories")
  .parse(process.argv);

const options = program.opts();

const port = options.port as number;
const initialPath = options.directory || process.cwd();
const secureMode = !!options.secure;

enum Mode {
  NavigateFiles,
  ConfirmTransfer,
  TransferFile,
}

type Context = {
  mode: Mode;
  path: string;
  socket: net.Socket;
  requestedFile?: string;
  totalBlocks: number;
  transferredBlocks: number;
  transferStartedAt: number;
  lastLoggedAt: number;
};

const log = (txt: string): void => console.log(`${new Date()} ${txt}`);
const write = (ctx: Context, txt: string): void => {
  ctx.socket.write(txt);
};
const writeln = (ctx: Context, txt: string = ''): void =>
  write(ctx, `${txt}\n\r`);

const isInRoot = (ctx: Context): boolean =>
  ctx.path === path.parse(ctx.path).root;
const getAbsoluteFilePath = (ctx: Context, fileName: string): string =>
  path.join(ctx.path, fileName);
const getFiles = (ctx: Context): string[] => {
  const files = fs.readdirSync(ctx.path).filter((file) => {
    if (file.startsWith('.')) {
      return false;
    }
    if (secureMode && isDirectory(ctx, file)) {
      return false;
    }
    return true;
  });
  const showParentDirectory = !isInRoot(ctx) && !secureMode;
  return showParentDirectory ? ['..', ...files] : files;
};
const isDirectory = (ctx: Context, filePath: string): boolean => {
  const absolutePath = getAbsoluteFilePath(ctx, filePath);
  const stat = fs.lstatSync(absolutePath);
  return stat.isSymbolicLink()
    ? fs.statSync(fs.realpathSync(absolutePath)).isDirectory()
    : stat.isDirectory();
};

const server = net.createServer((socket) => {
  const ctx: Context = {
    mode: Mode.NavigateFiles,
    path: initialPath,
    socket,
    totalBlocks: 0,
    transferredBlocks: 0,
    transferStartedAt: 0,
    lastLoggedAt: 0,
  };

  let inputBuffer = '';

  log('Client connected');

  listFiles(ctx);

  socket.on('data', (data) => {
    if (ctx.mode === Mode.NavigateFiles) {
      const input = data.toString();
      if (input.length === 0) {
        return;
      }
      if (input.trim().toLowerCase().startsWith('x')) {
        writeln(ctx, 'Goodbye!');
        socket.end();
        return;
      }
      if (input.trim().toLowerCase().startsWith('r')) {
        writeln(ctx, 'Refreshing...');
        listFiles(ctx);
        return;
      }

      for (let i = 0; i < input.length; i++) {
        const char = input[i];
        if (char === '\r' || char === '\n') {
          writeln(ctx);
          selectFile(ctx, parseInt(inputBuffer, 10));
          inputBuffer = '';
          break;
        } else if (inputBuffer.length && (char === '\b' || char === '\x7f')) {
          inputBuffer = inputBuffer.slice(0, -1);
          ctx.socket.write('\b \b');
        } else if (/[0-9\r\n\b]/.test(char)) {
          console.log('adding ' + char + ' to buffer');
          inputBuffer += char;
          ctx.socket.write(char);
        }
      }

      return;
    }

    if (ctx.mode === Mode.ConfirmTransfer) {
      confirmAndStartXModemTransfer(ctx, data.toString());
      return;
    }
  });

  socket.on('end', () => {
    socket.destroy();
    log('Client disconnected');
  });
});

function listFiles(ctx: Context) {
  const DIRECTORY_PREFIX = '<D>';
  const FILE_PREFIX = DIRECTORY_PREFIX.replace(/./g, '.');

  writeln(ctx, `----- ${ctx.path} -----`);

  try {
    ctx.mode = Mode.NavigateFiles;
    const files = getFiles(ctx);
    files.forEach((file, index) => {
      write(ctx, `${index + 1}`);
      write(ctx, ' ');
      write(ctx, isDirectory(ctx, file) ? DIRECTORY_PREFIX : FILE_PREFIX);
      write(ctx, ' ');
      writeln(ctx, file);
    });
    write(ctx, `Enter 1-${files.length}, R=refresh, X=exit: `);
  } catch (err) {
    console.error(err);
    writeln(ctx, `Error reading directory ${ctx.path}`);
  }
}

function selectFile(ctx: Context, fileNumber: number) {
  const filesOrDirs = getFiles(ctx);
  if (isNaN(fileNumber) || fileNumber < 1 || fileNumber > filesOrDirs.length) {
    writeln(
      ctx,
      `Invalid selection. Enter a number between 1-${filesOrDirs.length}.`
    );
    listFiles(ctx);
    return;
  }

  const selectedFileOrDir = filesOrDirs[fileNumber - 1];
  if (isDirectory(ctx, selectedFileOrDir)) {
    ctx.path = getAbsoluteFilePath(ctx, selectedFileOrDir);
    log(`Navigated to ${ctx.path}`);
    listFiles(ctx);
    return;
  }

  ctx.mode = Mode.ConfirmTransfer;
  ctx.requestedFile = getAbsoluteFilePath(ctx, selectedFileOrDir);
  write(ctx, `Start transferring ${ctx.requestedFile} via XMODEM? [Y/n]: `);
}

function confirmAndStartXModemTransfer(ctx: Context, input: string) {
  ctx.mode = Mode.TransferFile;

  const sanitizedInput = (input || '').trim().toLowerCase();
  if (sanitizedInput.length > 0 && !sanitizedInput.startsWith('y')) {
    writeln(ctx, 'No');
    listFiles(ctx);
    return;
  }

  writeln(ctx, 'Yes');
  writeln(ctx);

  writeln(ctx, `Initiating XMODEM transfer for ${ctx.requestedFile}`);
  writeln(ctx, 'Please start your XMODEM receiver NOW.');

  const filePath = ctx.requestedFile!;

  const buffer = fs.readFileSync(filePath);

  const x = new Xmodem(filePath);

  x.on('ready', (packagedBufferLength: number) => {
    log('Waiting for client to start XMODEM protocol...');
    ctx.totalBlocks = packagedBufferLength;
  });

  x.on('start', () => {
    ctx.transferStartedAt = Date.now();
    ctx.lastLoggedAt = ctx.transferStartedAt;
    ctx.transferredBlocks = 0;
    log(`Transfer started for ${ctx.requestedFile}`);
  });

  x.on('status', (statusObj: { signal: string; block: number }) => {
    const now = Date.now();
    if (now - ctx.lastLoggedAt < 5000) {
      return;
    }

    if (statusObj.signal !== 'SOH') {
      return;
    }

    const transferredBlocks = statusObj.block;
    const blocksRemaining = ctx.totalBlocks - transferredBlocks;
    const elapsedMilliseconds = now - ctx.transferStartedAt;
    const millisecondsForBlock = elapsedMilliseconds / transferredBlocks;
    const secondsRemaining = Math.round(
      (blocksRemaining * millisecondsForBlock) / 1000
    );
    const minutesLeft = Math.floor(secondsRemaining / 60);
    const secondsLeft = secondsRemaining % 60;
    const bytesPerSecond = Math.round(
      (transferredBlocks * 128) / (elapsedMilliseconds / 1000)
    );
    log(
      `Transferred ${transferredBlocks} of ${
        ctx.totalBlocks
      } blocks - ${bytesPerSecond}B/sec - ETA: ${
        minutesLeft ? minutesLeft + 'min ' : ''
      }${secondsLeft}sec`
    );

    ctx.transferredBlocks = transferredBlocks;
    ctx.lastLoggedAt = now;
  });

  x.on('stop', (exitCode: number) => {
    writeln(ctx);
    writeln(ctx);
    if (exitCode === 0) {
      const message = `Transfer of ${ctx.requestedFile} completed successfully`;
      log(message);
      writeln(ctx, message);
    } else {
      const message = `Transfer stopped with exit code ${exitCode}`;
      log(message);
      writeln(ctx, message);
    }
    listFiles(ctx);
  });

  x.send(ctx.socket, buffer);
}

function getServerIpAddress() {
  const interfaces = os.networkInterfaces();
  for (const name of Object.keys(interfaces)) {
    if (interfaces[name]) {
      for (const iface of interfaces[name]) {
        if (iface.family === 'IPv4' && !iface.internal) {
          return iface.address;
        }
      }
    }
  }
  return 'localhost';
}

server.listen(port, () => {
  log(`Server now listening in ${getServerIpAddress()}:${port}`);
});
