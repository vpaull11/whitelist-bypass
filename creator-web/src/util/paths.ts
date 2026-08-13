import * as path from 'path';
import * as fs from 'fs';

const isWindows = process.platform === 'win32';

export function binaryName(base: string): string {
  return isWindows ? base + '.exe' : base;
}

/**
 * Resolve the directory containing headless-*-creator binaries.
 * Priority:
 *   1. BINS_DIR env variable
 *   2. ../headless/ relative to project root (dev layout)
 *   3. /opt/wlb/bin (Docker / production layout)
 */
export function resolveBinsDir(): string {
  const envDir = process.env.BINS_DIR;
  if (envDir && fs.existsSync(envDir)) return envDir;

  const devDir = path.resolve(__dirname, '..', '..', '..', 'headless');
  if (fs.existsSync(devDir)) return devDir;

  const prodDir = '/opt/wlb/bin';
  if (fs.existsSync(prodDir)) return prodDir;

  return devDir; // fallback, may fail later
}

/**
 * Locate a headless binary. Searches for the exact name first, then tries
 * glob-style matches (e.g. headless-vk-creator* picks up the -bundle variant).
 */
export function resolveBinary(binsDir: string, name: string): string {
  const bin = binaryName(name);

  // Direct match (production layout — binaries in a flat directory)
  const flat = path.join(binsDir, bin);
  if (fs.existsSync(flat)) return flat;

  // Dev layout — binaries built inside platform subdirectories
  // e.g. headless/vk/headless-vk-creator.exe
  const platform = name
    .replace('headless-', '')
    .replace('-creator', '');
  const nested = path.join(binsDir, platform, bin);
  if (fs.existsSync(nested)) return nested;

  // Bundle variant (build-creator.sh renames to *-bundle[.exe])
  const bundleName = binaryName(name.replace('-creator', '-bundle'));
  const bundle = path.join(binsDir, bundleName);
  if (fs.existsSync(bundle)) return bundle;

  // Return flat path as fallback — caller will get a clear ENOENT
  return flat;
}

/** Resolve the data directory for persistent storage */
export function resolveDataDir(): string {
  const envDir = process.env.DATA_DIR;
  if (envDir) {
    fs.mkdirSync(envDir, { recursive: true });
    return path.resolve(envDir);
  }
  const defaultDir = path.resolve(__dirname, '..', '..', 'data');
  fs.mkdirSync(defaultDir, { recursive: true });
  return defaultDir;
}
