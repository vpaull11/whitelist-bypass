import * as fs from 'fs';
import * as path from 'path';
import { ConnectionConfig, AppSettings, UpstreamProxy } from './types';
import { resolveDataDir } from './util/paths';

const CONNECTIONS_FILE = 'connections.json';
const SETTINGS_FILE = 'settings.json';

export class Store {
  private dataDir: string;

  constructor() {
    this.dataDir = resolveDataDir();
  }

  get cookiesDir(): string {
    const dir = path.join(this.dataDir, 'cookies');
    fs.mkdirSync(dir, { recursive: true });
    return dir;
  }

  get sessionsDir(): string {
    const dir = path.join(this.dataDir, 'sessions');
    fs.mkdirSync(dir, { recursive: true });
    return dir;
  }

  // ── Connections ──────────────────────────────────────────────

  loadConnections(): ConnectionConfig[] {
    const filePath = path.join(this.dataDir, CONNECTIONS_FILE);
    try {
      const raw = fs.readFileSync(filePath, 'utf8');
      const parsed = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      return [];
    }
  }

  saveConnections(connections: ConnectionConfig[]): void {
    const filePath = path.join(this.dataDir, CONNECTIONS_FILE);
    fs.writeFileSync(filePath, JSON.stringify(connections, null, 2), 'utf8');
  }

  addConnection(config: ConnectionConfig): void {
    const connections = this.loadConnections();
    connections.push(config);
    this.saveConnections(connections);
  }

  updateConnection(id: string, update: Partial<ConnectionConfig>): ConnectionConfig | null {
    const connections = this.loadConnections();
    const idx = connections.findIndex((c) => c.id === id);
    if (idx === -1) return null;
    connections[idx] = { ...connections[idx], ...update, id };
    this.saveConnections(connections);
    return connections[idx];
  }

  removeConnection(id: string): boolean {
    const connections = this.loadConnections();
    const filtered = connections.filter((c) => c.id !== id);
    if (filtered.length === connections.length) return false;
    this.saveConnections(filtered);
    return true;
  }

  // ── Settings ────────────────────────────────────────────────

  loadSettings(): AppSettings {
    const filePath = path.join(this.dataDir, SETTINGS_FILE);
    const defaults: AppSettings = {
      upstreamProxy: {
        socks: process.env.UPSTREAM_SOCKS || '',
        user: process.env.UPSTREAM_USER || '',
        pass: process.env.UPSTREAM_PASS || '',
      },
      debugLogging: process.env.DEBUG === 'true',
      resources: process.env.RESOURCES || 'default',
      vkBot: {
        token: process.env.VK_BOT_TOKEN || '',
        groupId: process.env.VK_BOT_GROUP_ID || '',
        userIds: process.env.VK_BOT_USER_IDS || '',
      },
    };
    try {
      const raw = fs.readFileSync(filePath, 'utf8');
      const saved = JSON.parse(raw);
      return { ...defaults, ...saved };
    } catch {
      return defaults;
    }
  }

  saveSettings(settings: AppSettings): void {
    const filePath = path.join(this.dataDir, SETTINGS_FILE);
    fs.writeFileSync(filePath, JSON.stringify(settings, null, 2), 'utf8');
  }

  // ── Cookies ─────────────────────────────────────────────────

  cookieFilePath(platform: string): string {
    const COOKIE_FILES: Record<string, string> = {
      vk: 'cookies-vk.json',
      telemost: 'cookies-yandex.json',
      wbstream: 'cookies-wbstream.json',
      dion: 'cookies-dion.json',
    };
    return path.join(this.cookiesDir, COOKIE_FILES[platform] || `cookies-${platform}.json`);
  }

  listCookies(): Record<string, { exists: boolean; size: number; modifiedAt?: string }> {
    const platforms = ['vk', 'telemost', 'wbstream', 'dion'];
    const result: Record<string, { exists: boolean; size: number; modifiedAt?: string }> = {};
    for (const p of platforms) {
      const fp = this.cookieFilePath(p);
      try {
        const stat = fs.statSync(fp);
        result[p] = {
          exists: true,
          size: stat.size,
          modifiedAt: stat.mtime.toISOString(),
        };
      } catch {
        result[p] = { exists: false, size: 0 };
      }
    }
    return result;
  }

  saveCookieFile(platform: string, content: Buffer): void {
    const fp = this.cookieFilePath(platform);
    fs.writeFileSync(fp, content);
  }

  deleteCookieFile(platform: string): boolean {
    const fp = this.cookieFilePath(platform);
    try {
      fs.unlinkSync(fp);
      return true;
    } catch {
      return false;
    }
  }
}
