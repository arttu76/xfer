import * as fs from "fs";
import * as path from "path";
import { Context, Mode, GlobalConfig } from "./types";
import { write, writeln } from "./utils";
import { DIRECTORY_PREFIX, FILE_PREFIX } from "./constants";
import { logger } from "./logger";

// Utility functions for file navigation
export const isInRoot = (ctx: Context): boolean =>
  ctx.path === path.parse(ctx.path).root;

export const getAbsoluteFilePath = (ctx: Context, fileName: string, config?: GlobalConfig): string => {
  // Special case: ".." is allowed in non-secure mode for parent directory navigation
  if (fileName === ".." && config && !config.secureMode) {
    return path.resolve(ctx.path, "..");
  }
  
  // For secure mode or other files, prevent path traversal attacks
  const resolvedPath = path.resolve(ctx.path, fileName);
  
  // Only validate if in secure mode or if the path contains suspicious patterns
  if (config?.secureMode || fileName.includes("../") || fileName.includes("..\\")) {
    const normalizedBase = path.resolve(ctx.path);
    const normalizedResolved = path.resolve(resolvedPath);
    
    if (!normalizedResolved.startsWith(normalizedBase)) {
      throw new Error('Path traversal attempt detected');
    }
  }
  
  return resolvedPath;
};

export const getFiles = (ctx: Context, config: GlobalConfig): string[] => {
  try {
    const files = fs.readdirSync(ctx.path).filter((file) => {
    if (file.startsWith(".")) {
      return false;
    }
    if (config.secureMode && isDirectory(ctx, file)) {
      return false;
    }
    return true;
  });
  // Sort files case-insensitively
  files.sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()));
    const showParentDirectory = !isInRoot(ctx) && !config.secureMode;
    return showParentDirectory ? ["..", ...files] : files;
  } catch (err) {
    logger.error(`Error reading directory ${ctx.path}: ${err}`);
    return [];
  }
};

export const isDirectory = (ctx: Context, filePath: string): boolean => {
  try {
    // For isDirectory check, we don't need to pass config since we're just checking file type
    const absolutePath = path.resolve(ctx.path, filePath);
    const stat = fs.lstatSync(absolutePath);
    return stat.isSymbolicLink()
      ? fs.statSync(fs.realpathSync(absolutePath)).isDirectory()
      : stat.isDirectory();
  } catch (err) {
    logger.error(`Error checking if ${filePath} is directory: ${err}`);
    return false;
  }
};

export function listFiles(ctx: Context, config: GlobalConfig) {
  writeln(ctx, `----- ${ctx.path} -----`);

  try {
    ctx.mode = Mode.NavigateFiles;
    const files = getFiles(ctx, config);
    files.forEach((file, index) => {
      write(ctx, `${index + 1}`);
      write(ctx, " ");
      write(ctx, isDirectory(ctx, file) ? DIRECTORY_PREFIX : FILE_PREFIX);
      write(ctx, " ");
      writeln(ctx, file);
    });
    write(ctx, `Enter 1-${files.length}, R=refresh, X=exit: `);
  } catch (err) {
    logger.error(`Error reading directory ${ctx.path}: ${err}`);
    writeln(ctx, `Error reading directory ${ctx.path}`);
  }
}

export function selectFile(
  ctx: Context,
  fileNumber: number,
  config: GlobalConfig,
  onFileSelected: (ctx: Context) => void
) {
  const filesOrDirs = getFiles(ctx, config);
  if (isNaN(fileNumber) || fileNumber < 1 || fileNumber > filesOrDirs.length) {
    writeln(
      ctx,
      `Invalid selection. Enter a number between 1-${filesOrDirs.length}.`
    );
    listFiles(ctx, config);
    return;
  }

  const selectedFileOrDir = filesOrDirs[fileNumber - 1];
  if (isDirectory(ctx, selectedFileOrDir)) {
    ctx.path = getAbsoluteFilePath(ctx, selectedFileOrDir, config);
    logger.debug(`Navigated to ${ctx.path}`);
    listFiles(ctx, config);
    return;
  }

  ctx.mode = Mode.ConfirmTransfer;
  ctx.requestedFile = getAbsoluteFilePath(ctx, selectedFileOrDir, config);
  onFileSelected(ctx);
}