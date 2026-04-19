import * as net from "net";

export enum Mode {
  NavigateFiles,
  ConfirmTransfer,
  TransferFile,
}

export type Context = {
  mode: Mode;
  path: string;
  socket: net.Socket;
  requestedFile?: string;
  totalBlocks: number;
  transferredBlocks: number;
  transferStartedAt: number;
  lastLoggedAt: number;
};

export interface GlobalConfig {
  secureMode: boolean;
}