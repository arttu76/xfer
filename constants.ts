// Network constants
export const DEFAULT_PORT = 23;
export const MIN_PORT = 0;
export const MAX_PORT = 65535;

// Transfer constants
export const XMODEM_BLOCK_SIZE = 128;
export const TRANSFER_LOG_INTERVAL_MS = 5000;

// UI constants
export const DIRECTORY_PREFIX = "<D>";
export const FILE_PREFIX = "   ";

// Signals
export const XMODEM_SOH_SIGNAL = "SOH";

// Network interfaces
export const IPV4_FAMILY = "IPv4";

// Protocol control characters
export const CAN_BYTE = 0x18; // ASCII CAN (Cancel) character - Ctrl+X
export const ZMODEM_CANCEL_COUNT = 5; // Number of consecutive CAN bytes to cancel ZMODEM