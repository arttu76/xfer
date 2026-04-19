export enum LogLevel {
  ERROR = 0,
  WARN = 1,
  INFO = 2,
  DEBUG = 3,
}

class Logger {
  private logLevel: LogLevel = LogLevel.INFO;

  setLogLevel(level: LogLevel): void {
    this.logLevel = level;
  }

  private formatMessage(level: string, message: string): string {
    return `${new Date().toISOString()} [${level}] ${message}`;
  }

  error(message: string): void {
    if (this.logLevel >= LogLevel.ERROR) {
      console.error(this.formatMessage('ERROR', message));
    }
  }

  warn(message: string): void {
    if (this.logLevel >= LogLevel.WARN) {
      console.warn(this.formatMessage('WARN', message));
    }
  }

  info(message: string): void {
    if (this.logLevel >= LogLevel.INFO) {
      console.log(this.formatMessage('INFO', message));
    }
  }

  debug(message: string): void {
    if (this.logLevel >= LogLevel.DEBUG) {
      console.log(this.formatMessage('DEBUG', message));
    }
  }

  // Specialized logging for transfer status
  transferStatus(protocol: string, message: string): void {
    this.info(`${protocol}: ${message}`);
  }
}

// Export singleton instance
export const logger = new Logger();