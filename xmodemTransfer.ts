import * as fs from "fs";
import * as path from "path";
import { Xmodem } from "xmodem.ts";
import { Context, Mode, GlobalConfig } from "./types";
import { md5Of, writeln } from "./utils";
import {
  showTransferInitiation,
  logTransferStatus,
  showTransferComplete,
} from "./protocolSelector";
import { XMODEM_BLOCK_SIZE, TRANSFER_LOG_INTERVAL_MS, XMODEM_SOH_SIGNAL } from "./constants";

export function startXModemTransfer(ctx: Context, config: GlobalConfig) {
  ctx.mode = Mode.TransferFile;
  writeln(ctx);

  if (!ctx.requestedFile) {
    writeln(ctx, "Error: No file selected for transfer");
    return;
  }

  const filePath = ctx.requestedFile;

  let buffer: Buffer;
  try {
    buffer = fs.readFileSync(filePath);
  } catch (err) {
    const errorMessage = err instanceof Error ? err.message : 'Unknown error';
    logTransferStatus("XMODEM", `Error reading file: ${errorMessage}`);
    writeln(ctx, `Error reading file: ${errorMessage}`);
    showTransferComplete(ctx, config, "XMODEM", false);
    return;
  }

  const blocks = Math.ceil(buffer.length / XMODEM_BLOCK_SIZE);

  const fileName = path.basename(filePath);
  writeln(ctx, `Ready to download ${fileName}`);
  writeln(ctx, `MD5: ${md5Of(buffer)}`);
  showTransferInitiation(ctx, "XMODEM", `for ${blocks} blocks`);

  const x = new Xmodem(filePath);

  x.on("ready", (packagedBufferLength: number) => {
    logTransferStatus("XMODEM", "Waiting for client to start protocol...");
    ctx.totalBlocks = packagedBufferLength;
  });

  x.on("start", () => {
    ctx.transferStartedAt = Date.now();
    ctx.lastLoggedAt = ctx.transferStartedAt;
    ctx.transferredBlocks = 0;
    logTransferStatus("XMODEM", `Transfer started for ${ctx.requestedFile}`);
  });

  x.on("status", (statusObj: { signal: string; block: number }) => {
    const now = Date.now();
    if (now - ctx.lastLoggedAt < TRANSFER_LOG_INTERVAL_MS) {
      return;
    }

    if (statusObj.signal !== XMODEM_SOH_SIGNAL) {
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
      (transferredBlocks * XMODEM_BLOCK_SIZE) / (elapsedMilliseconds / 1000)
    );
    logTransferStatus(
      "XMODEM",
      `Transferred ${transferredBlocks} of ${
        ctx.totalBlocks
      } blocks - ${bytesPerSecond}B/sec - ETA: ${
        minutesLeft ? minutesLeft + "min " : ""
      }${secondsLeft}sec`
    );

    ctx.transferredBlocks = transferredBlocks;
    ctx.lastLoggedAt = now;
  });

  x.on("stop", (exitCode: number) => {
    showTransferComplete(ctx, config, "XMODEM", exitCode === 0, exitCode);
  });

  x.send(ctx.socket, buffer);
}