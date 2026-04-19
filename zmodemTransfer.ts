import * as fs from "fs";
import * as path from "path";
import { Context, Mode, GlobalConfig } from "./types";
import { md5Of, sleep, TERMINAL_MODE_FLUSH_MS, writeln } from "./utils";
import {
  showTransferInitiation,
  logTransferStatus,
  showTransferComplete,
} from "./protocolSelector";
import { sendBufferViaZmodem, ZmodemCancelledError } from "./zmodemEngine";

export async function startZModemTransfer(ctx: Context, config: GlobalConfig) {
  ctx.mode = Mode.TransferFile;
  writeln(ctx);

  if (!ctx.requestedFile) {
    writeln(ctx, "Error: No file selected for transfer");
    showTransferComplete(ctx, config, "ZMODEM", false);
    return;
  }

  const filePath = ctx.requestedFile;

  let buffer: Buffer;
  try {
    buffer = fs.readFileSync(filePath);
  } catch (err) {
    const errorMessage = err instanceof Error ? err.message : "Unknown error";
    logTransferStatus("ZMODEM", `Error reading file: ${errorMessage}`);
    writeln(ctx, `Error reading file: ${errorMessage}`);
    showTransferComplete(ctx, config, "ZMODEM", false);
    return;
  }

  const fileName = path.basename(filePath);
  const fileMd5 = md5Of(buffer);

  writeln(ctx, `Ready to download ${fileName}`);
  writeln(ctx, `MD5: ${fileMd5}`);
  showTransferInitiation(ctx, "ZMODEM");

  // Flush window so the MD5 line above doesn't end up in the same serial
  // read as the ZRQINIT pattern — xprzmodem's host monitor discards any
  // bytes before the match and the text would vanish on the Amiga side.
  await sleep(TERMINAL_MODE_FLUSH_MS);

  // The engine takes full ownership of socket data while transferring. Stash
  // any existing listeners (server.ts attaches one for navigation mode) and
  // reinstall them after the transfer settles.
  const originalDataListeners = ctx.socket.listeners("data") as ((...args: any[]) => void)[];
  ctx.socket.removeAllListeners("data");

  const restoreListeners = () => {
    for (const listener of originalDataListeners) {
      ctx.socket.on("data", listener as any);
    }
  };

  try {
    await sendBufferViaZmodem(ctx.socket, buffer, fileName);
    // Flush window on the way out too, before the file listing is printed.
    await sleep(TERMINAL_MODE_FLUSH_MS);
    restoreListeners();
    logTransferStatus("ZMODEM", "Transfer completed successfully");
    showTransferComplete(ctx, config, "ZMODEM", true);
  } catch (err) {
    await sleep(TERMINAL_MODE_FLUSH_MS);
    restoreListeners();
    if (err instanceof ZmodemCancelledError) {
      writeln(ctx);
      writeln(ctx, "Transfer cancelled.");
      logTransferStatus("ZMODEM", "Transfer cancelled by user");
      showTransferComplete(ctx, config, "ZMODEM", false);
    } else {
      const errorMessage = err instanceof Error ? err.message : "Unknown error";
      logTransferStatus("ZMODEM", errorMessage);
      showTransferComplete(ctx, config, "ZMODEM", false);
    }
  }
}
