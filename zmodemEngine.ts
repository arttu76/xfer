import { Socket } from "net";
import { Sender, SenderEvent, ZDLE_TABLE, crc16Xmodem } from "zmodem2";
import { logger } from "./logger";

// Ported from the telnet project's ZMODEM implementation. zmodem2 always uses
// ZBIN32 (CRC32) for ZFILE/ZDATA/ZEOF and advertises no option to downgrade.
// Term 4.8 advertises CANFC32 in its ZRINIT but hangs when it actually
// receives a CRC32 frame (documented on amigalove.com). It also expects the
// lrzsz-style fileinfo block (name + size/mtime/mode/...) not just name+size.
// We override the private queueZfile/queueZdata/queueZeof methods with a
// CRC16 (ZBIN) implementation that matches lrzsz's wire format byte-for-byte.
// Receivers that handle CRC32 fine (all modern tools, rz) also handle CRC16.

const log = (msg: string): void => logger.info(msg);
const logError = (msg: string, err?: unknown): void =>
  logger.error(err ? `${msg}: ${err instanceof Error ? err.message : err}` : msg);

type SenderInternals = {
  fileName: string;
  fileSize: number;
  outgoing: { clear(): void; extend(bytes: number[] | Uint8Array): void };
  outgoingOffset: number;
  maxSubpacketSize: number;
  maxSubpacketsPerAck: number;
  updateReceiverCaps: (header: unknown) => void;
  queueZfile: () => void;
  queueZdata: (offset: number, data: Uint8Array, kind: number, includeHeader: boolean) => void;
  queueZeof: (offset: number) => void;
};

// xprzmodem.library (Term 4.8, NComm, etc.) hard-codes KSIZE=1024 as its
// zrdata buffer. zmodem2 always sends 8192-byte subpackets and ignores the
// receiver's ZRINIT buffer field, so xprzmodem hits "Data packet too long",
// sends its attn string + ZRPOS(0), and the transfer loops forever.
const SUBPACKET_SIZE = 1024;

// zmodem2 defaults to 200 subpackets (~200 KB) per ACK when CANOVIO is set.
// Shrinking the frame to 8 subpackets (~8 KB) forces a real end-to-end ACK
// round-trip every 8 KB, which naturally paces to the actual wire speed —
// critical for slow serial clients (Amiga 600 @ 19200 baud through a WiFi
// modem) whose internal buffer is only a few KB.
const SUBPACKETS_PER_ACK = 8;

function appendEscaped(out: number[], bytes: Uint8Array | number[]): void {
  for (const b of bytes) {
    const esc = ZDLE_TABLE[b];
    if (esc !== b) out.push(0x18);
    out.push(esc);
  }
}

function buildZbinHeader(frame: number, count: number): number[] {
  // ZBIN header: ZPAD(0x2a) ZDLE(0x18) 'A'(0x41) escaped(5-byte payload) escaped(2-byte CRC16 big-endian)
  const out: number[] = [0x2a, 0x18, 0x41];
  const payload = new Uint8Array([
    frame,
    count & 0xff,
    (count >>> 8) & 0xff,
    (count >>> 16) & 0xff,
    (count >>> 24) & 0xff,
  ]);
  appendEscaped(out, payload);
  const crc = crc16Xmodem(payload);
  appendEscaped(out, [(crc >>> 8) & 0xff, crc & 0xff]);
  return out;
}

function patchSenderForCrc16(sender: Sender): void {
  const s = sender as unknown as SenderInternals;

  s.maxSubpacketSize = SUBPACKET_SIZE;
  s.maxSubpacketsPerAck = SUBPACKETS_PER_ACK;
  const origUpdateCaps = s.updateReceiverCaps;
  s.updateReceiverCaps = function (header: unknown) {
    origUpdateCaps.call(this, header);
    s.maxSubpacketSize = SUBPACKET_SIZE;
    s.maxSubpacketsPerAck = SUBPACKETS_PER_ACK;
  };

  s.queueZfile = function () {
    // Header: ZBIN ZFILE frame with ZF3..ZF0 = 00 00 00 01 (ZCBIN binary)
    const out: number[] = [0x2a, 0x18, 0x41];
    const headerPayload = new Uint8Array([0x04, 0x00, 0x00, 0x00, 0x01]);
    appendEscaped(out, headerPayload);
    const hCrc = crc16Xmodem(headerPayload);
    appendEscaped(out, [(hCrc >>> 8) & 0xff, hCrc & 0xff]);
    // Subpacket: "name\0size mtime mode serial files bytes\0"
    const info: number[] = [];
    for (let i = 0; i < s.fileName.length; i++) info.push(s.fileName.charCodeAt(i));
    info.push(0);
    const mtimeOctal = Math.floor(Date.now() / 1000).toString(8);
    const meta = `${s.fileSize} ${mtimeOctal} 100644 0 1 ${s.fileSize}`;
    for (let i = 0; i < meta.length; i++) info.push(meta.charCodeAt(i));
    info.push(0);
    appendEscaped(out, info);
    // ZDLE + ZCRCW + CRC16 of (info + ZCRCW)
    out.push(0x18, 0x6b);
    const crcInput = new Uint8Array([...info, 0x6b]);
    const subCrc = crc16Xmodem(crcInput);
    appendEscaped(out, [(subCrc >>> 8) & 0xff, subCrc & 0xff]);
    s.outgoing.clear();
    s.outgoing.extend(out);
    s.outgoingOffset = 0;
  };

  s.queueZdata = function (offset, data, kind, includeHeader) {
    const out: number[] = [];
    if (includeHeader) out.push(...buildZbinHeader(0x0a, offset)); // ZDATA frame = 10
    appendEscaped(out, data);
    out.push(0x18, kind);
    const crcInput = new Uint8Array(data.length + 1);
    crcInput.set(data);
    crcInput[data.length] = kind;
    const crc = crc16Xmodem(crcInput);
    appendEscaped(out, [(crc >>> 8) & 0xff, crc & 0xff]);
    s.outgoing.clear();
    s.outgoing.extend(out);
    s.outgoingOffset = 0;
  };

  s.queueZeof = function (offset) {
    const header = buildZbinHeader(0x0b, offset); // ZEOF frame = 11
    s.outgoing.clear();
    s.outgoing.extend(header);
    s.outgoingOffset = 0;
  };
}

// ZMODEM ESCCTL compliance. zmodem2 ignores the receiver's ESCCTL flag in
// ZRINIT entirely, so we toggle it ourselves by patching ZDLE_TABLE before
// queueing frames. Retro terminals that DON'T set ESCCTL (Amiga Term 4.8
// sends caps=0x23) reject frames whose headers contain ZDLE-escaped control
// bytes they didn't ask for. Terminals that DO set ESCCTL (lrzsz --escape,
// sends caps=0x63) require those escapes.
const ESCCTL_PATCH: Array<[number, number]> = [];
for (let i = 0; i < 0x20; i++) {
  if (ZDLE_TABLE[i] === i) ESCCTL_PATCH.push([i, i ^ 0x40]);
  const hi = i | 0x80;
  if (ZDLE_TABLE[hi] === hi) ESCCTL_PATCH.push([hi, hi ^ 0x40]);
}
function enableEscctl(): void {
  for (const [idx, val] of ESCCTL_PATCH) ZDLE_TABLE[idx] = val;
}
function disableEscctl(): void {
  for (const [idx] of ESCCTL_PATCH) ZDLE_TABLE[idx] = idx;
}
disableEscctl();

function detectEscctlFromZrinit(sniff: readonly number[]): boolean | null {
  if (sniff.length < 18) return null;
  for (let i = 0; i <= sniff.length - 18; i++) {
    if (sniff[i] !== 0x2a || sniff[i + 1] !== 0x2a || sniff[i + 2] !== 0x18 || sniff[i + 3] !== 0x42) continue;
    let hexOk = true;
    for (let j = 4; j < 18; j++) {
      const c = sniff[i + j];
      if (!((c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x46) || (c >= 0x61 && c <= 0x66))) {
        hexOk = false;
        break;
      }
    }
    if (!hexOk) continue;
    const frame = parseInt(String.fromCharCode(sniff[i + 4], sniff[i + 5]), 16);
    if (frame !== 1) continue;
    const f3 = parseInt(String.fromCharCode(sniff[i + 12], sniff[i + 13]), 16);
    return (f3 & 0x40) !== 0;
  }
  return null;
}

const ACTIVITY_TIMEOUT_MS = 60000;
const CAN = 0x18;
const CAN_CANCEL_THRESHOLD = 5;
// Standard lrzsz cancel echo: 8x CAN + 10x backspace.
const CANCEL_ECHO = Buffer.from([
  CAN, CAN, CAN, CAN, CAN, CAN, CAN, CAN,
  0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08,
]);

export class ZmodemCancelledError extends Error {
  constructor() {
    super("ZMODEM transfer cancelled by receiver");
    this.name = "ZmodemCancelledError";
  }
}

async function sendPreparedBuffer(socket: Socket, buffer: Buffer, filename: string): Promise<void> {
  let fileBuffer: Buffer | null = buffer;
  const sender = new Sender();
  patchSenderForCrc16(sender);

  return new Promise<void>((resolve, reject) => {
    let settled = false;
    let activityTimeoutId: NodeJS.Timeout;
    let consecutiveCan = 0;
    let escctlApplied = false;
    let backpressure = false;
    const sniff: number[] = [];
    const startTime = Date.now();

    const maybeNegotiateEscctl = (chunk: Buffer): void => {
      if (escctlApplied) return;
      for (const b of chunk) {
        sniff.push(b);
        if (sniff.length > 24) sniff.shift();
      }
      const requested = detectEscctlFromZrinit(sniff);
      if (requested === null) return;
      if (requested) enableEscctl();
      escctlApplied = true;
    };

    const cleanup = () => {
      fileBuffer = null;
      clearTimeout(activityTimeoutId);
      socket.removeListener("close", onSocketClose);
      socket.removeListener("error", onSocketError);
      socket.removeListener("data", onData);
      socket.removeListener("drain", onDrain);
      disableEscctl();
    };

    const settle = (fn: () => void) => {
      if (settled) return;
      settled = true;
      cleanup();
      fn();
    };

    const resetActivityTimeout = () => {
      clearTimeout(activityTimeoutId);
      activityTimeoutId = setTimeout(() => {
        settle(() => {
          logError("ZMODEM activity timeout - no progress");
          reject(new Error("ZMODEM activity timeout - no progress"));
        });
      }, ACTIVITY_TIMEOUT_MS);
    };

    const onSocketClose = () => {
      settle(() => {
        logError("ZMODEM transfer aborted - client disconnected");
        reject(new Error("ZMODEM transfer aborted - client disconnected"));
      });
    };

    const onSocketError = (err: Error) => {
      settle(() => {
        logError("ZMODEM transfer aborted - socket error", err);
        reject(new Error("ZMODEM transfer aborted - socket error"));
      });
    };

    const sendOut = (out: Uint8Array) => {
      if (out.length === 0) return;
      if (!socket.write(Buffer.from(out))) backpressure = true;
    };

    const onDrain = () => {
      if (settled || !backpressure) return;
      backpressure = false;
      processAll();
    };

    const processAll = () => {
      if (settled || backpressure) return;

      sendOut(sender.drainOutgoing());
      if (backpressure) return;

      let req = sender.pollFile();
      while (req !== null) {
        const chunk = fileBuffer!.subarray(req.offset, req.offset + req.len);
        sender.feedFile(chunk);
        sendOut(sender.drainOutgoing());
        if (backpressure) return;
        req = sender.pollFile();
      }

      let evt = sender.pollEvent();
      while (evt !== null) {
        if (evt === SenderEvent.FileComplete) {
          sender.finishSession();
          sendOut(sender.drainOutgoing());
        } else if (evt === SenderEvent.SessionComplete) {
          sendOut(sender.drainOutgoing());
          const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
          log(`ZMODEM transfer completed in ${elapsed}s`);
          settle(() => resolve());
        }
        evt = sender.pollEvent();
      }
    };

    const onData = (buf: Buffer) => {
      if (settled) return;

      maybeNegotiateEscctl(buf);

      for (const byte of buf) {
        if (byte === CAN) {
          if (++consecutiveCan >= CAN_CANCEL_THRESHOLD) {
            settle(() => {
              log("ZMODEM transfer cancelled by receiver");
              if (!socket.destroyed) socket.write(CANCEL_ECHO);
              reject(new ZmodemCancelledError());
            });
            return;
          }
        } else {
          consecutiveCan = 0;
        }
      }

      try {
        sender.feedIncoming(new Uint8Array(buf));
        processAll();
        resetActivityTimeout();
      } catch (err) {
        settle(() => {
          logError("ZMODEM protocol error", err);
          reject(err instanceof Error ? err : new Error("ZMODEM protocol error"));
        });
      }
    };

    socket.on("close", onSocketClose);
    socket.on("error", onSocketError);
    socket.on("data", onData);
    socket.on("drain", onDrain);

    log(`Starting ZMODEM transfer: ${filename} (${fileBuffer!.length} bytes)`);

    // "rz\r" trigger before ZRQINIT for terminals that only auto-arm their
    // ZMODEM receiver on that specific string (Amiga NComm, Term, etc.).
    if (!socket.destroyed) socket.write("rz\r");

    sender.startFile(filename, fileBuffer!.length);
    processAll();
    resetActivityTimeout();
  });
}

export async function sendBufferViaZmodem(
  socket: Socket,
  buffer: Buffer,
  filename: string
): Promise<void> {
  try {
    return await sendPreparedBuffer(socket, buffer, filename);
  } catch (error) {
    if (!(error instanceof ZmodemCancelledError)) {
      logError("Error in ZMODEM buffer transfer", error);
    }
    throw error;
  }
}
