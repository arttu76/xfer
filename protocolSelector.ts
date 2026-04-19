import { Context, Mode, GlobalConfig } from "./types";
import { write, writeln } from "./utils";
import { listFiles } from "./fileNavigator";
import { logger } from "./logger";

// Common prompt functions
export const showProtocolPrompt = (ctx: Context, _config: GlobalConfig): void => {
  write(ctx, `Transfer ${ctx.requestedFile} via [X]MODEM, [Z]MODEM, or [C]ancel?: `);
};

export const showTransferInitiation = (
  ctx: Context,
  protocol: string,
  additionalInfo?: string
): void => {
  writeln(ctx, `Initiating ${protocol} transfer for ${ctx.requestedFile}`);
  const suffix = additionalInfo ? ` ${additionalInfo}` : "";
  writeln(ctx, `Please start your ${protocol} receiver NOW${suffix}.`);
};

export const logTransferStatus = (protocol: string, message: string): void => {
  logger.transferStatus(protocol, message);
};

export const showTransferComplete = (
  ctx: Context,
  config: GlobalConfig,
  protocol: string,
  success: boolean,
  exitCode?: number
): void => {
  writeln(ctx);
  writeln(ctx);
  if (success) {
    const message = `${protocol} transfer of ${ctx.requestedFile} completed successfully`;
    logger.info(message);
    writeln(ctx, message);
  } else {
    const message = `${protocol} transfer stopped with exit code ${exitCode}`;
    logger.info(message);
    writeln(ctx, message);
  }
  listFiles(ctx, config);
};

export function confirmAndStartTransfer(
  ctx: Context,
  input: string,
  config: GlobalConfig,
  startXModem: (ctx: Context) => void,
  startZModem: (ctx: Context) => void
) {
  const sanitizedInput = (input || "").trim().toLowerCase();
  
  if (sanitizedInput.length === 0) {
    writeln(ctx, "");
    showProtocolPrompt(ctx, config);
    return;
  }

  if (sanitizedInput.startsWith("x")) {
    writeln(ctx, "XMODEM");
    startXModem(ctx);
    return;
  }
  
  if (sanitizedInput.startsWith("z")) {
    writeln(ctx, "ZMODEM");
    startZModem(ctx);
    return;
  }
  
  if (sanitizedInput.startsWith("c") || sanitizedInput.startsWith("n")) {
    writeln(ctx, "Cancelled");
    listFiles(ctx, config);
    return;
  }
  
  writeln(ctx, "Invalid option. Please enter X, Z, or C.");
  showProtocolPrompt(ctx, config);
}