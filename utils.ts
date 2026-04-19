import * as crypto from "crypto";
import { Context } from "./types";

// UI utility functions
export const write = (ctx: Context, txt: string): void => {
  ctx.socket.write(txt);
};

export const writeln = (ctx: Context, txt: string = ""): void =>
  write(ctx, `${txt}\r\n`);

export const md5Of = (buffer: Buffer): string =>
  crypto.createHash("md5").update(buffer).digest("hex");

export const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

// xprzmodem's host monitor (Term 4.8, NComm etc.) discards any bytes in the
// same serial read as the ZMODEM trigger pattern, and takes a moment to
// return from ZMODEM mode back to terminal mode. A short pause on each side
// of the transfer lets the client flush each transition in a separate read
// cycle, so the "MD5:" line before and the listing after don't get eaten.
export const TERMINAL_MODE_FLUSH_MS = 500;