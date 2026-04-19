import * as net from "net";
import * as os from "os";
import * as fs from "fs";
import * as path from "path";
import { Command, InvalidArgumentError } from "commander";
import { Context, Mode, GlobalConfig } from "./types";
import { writeln } from "./utils";
import { listFiles, selectFile } from "./fileNavigator";
import { showProtocolPrompt, confirmAndStartTransfer } from "./protocolSelector";
import { DEFAULT_PORT, MIN_PORT, MAX_PORT, IPV4_FAMILY } from "./constants";
import { logger } from "./logger";
import { startXModemTransfer } from "./xmodemTransfer";
import { startZModemTransfer } from "./zmodemTransfer";

const program = new Command();

program
  .name("xfer")
  .description(
    "Start XFER on your computer to allow (retro?) computers to download files with devices like WiModem232."
  )
  .version("1.0.4");

program
  .option(
    "-p, --port <number>",
    "port to use",
    (value: string): number => {
      const port = parseInt(value, 10);
      if (isNaN(port)) {
        throw new InvalidArgumentError("Not a number.");
      }
      if (port < MIN_PORT || port > MAX_PORT) {
        throw new InvalidArgumentError(`Port must be between ${MIN_PORT} and ${MAX_PORT}.`);
      }
      return port;
    },
    DEFAULT_PORT
  )
  .option(
    "-d, --directory <string>",
    "directory to serve",
    (value: string): string => {
      const errorMessage = `${value} is not a valid directory.`;
      try {
        const resolvedPath = path.resolve(value);
        const stat = fs.statSync(resolvedPath);
        if (!stat.isDirectory()) {
          throw new InvalidArgumentError(errorMessage);
        }
      } catch (error) {
        throw new InvalidArgumentError(errorMessage);
      }
      return value;
    },
    process.cwd()
  )
  .option("-s, --secure", "secure mode: don't allow user to change directories")
  .parse(process.argv);

const options = program.opts();

const port = options.port as number;
const initialPath = options.directory || process.cwd();

// Global configuration
const config: GlobalConfig = {
  secureMode: !!options.secure,
};

const log = (txt: string): void => logger.info(txt);

const server = net.createServer((socket) => {
  const ctx: Context = {
    mode: Mode.NavigateFiles,
    path: initialPath,
    socket,
    requestedFile: undefined,
    totalBlocks: 0,
    transferredBlocks: 0,
    transferStartedAt: 0,
    lastLoggedAt: 0,
  };

  let inputBuffer = "";

  log("Client connected");

  listFiles(ctx, config);

  socket.on("data", (data) => {
    if (ctx.mode === Mode.NavigateFiles) {
      const input = data.toString();
      if (input.length === 0) {
        return;
      }
      if (input.trim().toLowerCase().startsWith("x")) {
        writeln(ctx, "Goodbye!");
        socket.end();
        return;
      }
      if (input.trim().toLowerCase().startsWith("r")) {
        writeln(ctx, "Refreshing...");
        listFiles(ctx, config);
        return;
      }

      for (let i = 0; i < input.length; i++) {
        const char = input[i];
        if (char === "\r" || char === "\n") {
          writeln(ctx);
          selectFile(ctx, parseInt(inputBuffer, 10), config, () => {
            showProtocolPrompt(ctx, config);
          });
          inputBuffer = "";
          break;
        } else if (inputBuffer.length && (char === "\b" || char === "\x7f")) {
          inputBuffer = inputBuffer.slice(0, -1);
          ctx.socket.write("\b \b");
        } else if (/[0-9]/.test(char)) {
          inputBuffer += char;
          ctx.socket.write(char);
        }
      }

      return;
    }

    if (ctx.mode === Mode.ConfirmTransfer) {
      confirmAndStartTransfer(
        ctx,
        data.toString(),
        config,
        (ctx) => startXModemTransfer(ctx, config),
        (ctx) => startZModemTransfer(ctx, config)
      );
      return;
    }
  });

  socket.on("end", () => {
    socket.destroy();
    log("Client disconnected");
  });
});

function getServerIpAddress() {
  const interfaces = os.networkInterfaces();
  for (const name of Object.keys(interfaces)) {
    if (interfaces[name]) {
      for (const iface of interfaces[name]) {
        if (iface.family === IPV4_FAMILY && !iface.internal) {
          return iface.address;
        }
      }
    }
  }
  return "localhost";
}

server.listen(port, () => {
  log(`Server now listening in ${getServerIpAddress()}:${port}`);
});
