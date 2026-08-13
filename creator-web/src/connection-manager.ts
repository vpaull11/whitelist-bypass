import { spawn, ChildProcess } from 'child_process';
import * as net from 'net';
import * as fs from 'fs';
import * as crypto from 'crypto';
import {
  ConnectionConfig,
  ConnectionState,
  ConnectionStatus,
  ConnectionInfo,
  Platform,
  HeadlessMode,
  UpstreamProxy,
} from './types';
import {
  INITIAL_PORT_BASE,
  MAX_CRASH_RESTARTS,
  RESTART_DELAY_MS,
  MAX_LOG_LINES,
  HeadlessLogMarker,
  PLATFORM_JOIN_FLAG,
  PLATFORM_BINARY,
} from './constants';
import { Store } from './store';
import { resolveBinsDir, resolveBinary } from './util/paths';

export type ConnectionEventCallback = (
  type: 'update' | 'log',
  connectionId: string,
  data: unknown,
) => void;

export class ConnectionManager {
  private connections = new Map<string, ConnectionState>();
  private nextPortBase = INITIAL_PORT_BASE;
  private binsDir: string;
  private store: Store;
  private upstreamProxy: UpstreamProxy = { socks: '', user: '', pass: '' };
  private debugLogging = false;
  private resources = 'default';
  private onEvent: ConnectionEventCallback = () => {};

  constructor(store: Store) {
    this.store = store;
    this.binsDir = resolveBinsDir();
    const settings = store.loadSettings();
    this.upstreamProxy = settings.upstreamProxy;
    this.debugLogging = settings.debugLogging;
    this.resources = settings.resources;
    console.log(`[CM] bins dir: ${this.binsDir}`);
  }

  setEventCallback(cb: ConnectionEventCallback): void {
    this.onEvent = cb;
  }

  setUpstreamProxy(proxy: UpstreamProxy): void {
    this.upstreamProxy = {
      socks: (proxy?.socks || '').trim(),
      user: (proxy?.user || '').trim(),
      pass: (proxy?.pass || '').trim(),
    };
  }

  setDebugLogging(enabled: boolean): void {
    this.debugLogging = enabled;
  }

  setResources(mode: string): void {
    this.resources = mode;
  }

  // ── Public API ──────────────────────────────────────────────

  async createConnection(
    alias: string,
    platform: Platform,
    joinLink?: string,
    autoRestart = true,
  ): Promise<ConnectionInfo> {
    const id = crypto.randomUUID().slice(0, 8);
    const config: ConnectionConfig = {
      id,
      alias,
      platform,
      joinLink: joinLink || undefined,
      autoRestart,
      createdAt: new Date().toISOString(),
    };

    const ports = await this.allocPorts();
    const state: ConnectionState = {
      config,
      status: ConnectionStatus.Saved,
      process: null,
      tunnelConnected: false,
      crashCount: 0,
      logs: [],
      dcPort: ports.dc,
      pionPort: ports.pion,
      clientConnected: false,
      activeConns: 0,
      recvMB: 0,
      sendMB: 0,
    };

    this.connections.set(id, state);
    this.store.addConnection(config);

    return this.toConnectionInfo(state);
  }

  async startConnection(id: string): Promise<boolean> {
    const state = this.connections.get(id);
    if (!state) return false;
    await this.launchHeadless(id, state);
    return true;
  }

  stopConnection(id: string): boolean {
    const state = this.connections.get(id);
    if (!state) return false;
    this.killProcess(id, state);
    state.status = ConnectionStatus.Stopped;
    state.tunnelConnected = false;
    this.emitUpdate(id);
    return true;
  }

  async deleteConnection(id: string): Promise<boolean> {
    const state = this.connections.get(id);
    if (!state) return false;
    this.killProcess(id, state);
    this.connections.delete(id);
    this.store.removeConnection(id);
    return true;
  }

  updateConnection(id: string, update: { alias?: string; autoRestart?: boolean; joinLink?: string }): ConnectionInfo | null {
    const state = this.connections.get(id);
    if (!state) return null;
    if (update.alias !== undefined) state.config.alias = update.alias;
    if (update.autoRestart !== undefined) state.config.autoRestart = update.autoRestart;
    if (update.joinLink !== undefined) state.config.joinLink = update.joinLink;
    this.store.updateConnection(id, state.config);
    this.emitUpdate(id);
    return this.toConnectionInfo(state);
  }

  getConnection(id: string): ConnectionInfo | null {
    const state = this.connections.get(id);
    return state ? this.toConnectionInfo(state) : null;
  }

  getAllConnections(): ConnectionInfo[] {
    const result: ConnectionInfo[] = [];
    this.connections.forEach((state) => {
      result.push(this.toConnectionInfo(state));
    });
    return result;
  }

  getLogs(id: string): string[] {
    return this.connections.get(id)?.logs || [];
  }

  /**
   * Restore saved connections on startup.
   * Connections with autoRestart=true are launched automatically.
   */
  async restoreConnections(): Promise<void> {
    const saved = this.store.loadConnections();
    if (saved.length === 0) return;

    console.log(`[CM] Restoring ${saved.length} saved connection(s)...`);

    for (const config of saved) {
      if (this.connections.has(config.id)) continue;

      const ports = await this.allocPorts();
      const state: ConnectionState = {
        config,
        status: ConnectionStatus.Saved,
        process: null,
        tunnelConnected: false,
        crashCount: 0,
        logs: [],
        dcPort: ports.dc,
        pionPort: ports.pion,
        clientConnected: false,
        activeConns: 0,
        recvMB: 0,
        sendMB: 0,
      };
      this.connections.set(config.id, state);

      if (config.autoRestart) {
        this.addLog(config.id, `Restoring connection "${config.alias}" (${config.platform})...`);
        // Stagger restarts slightly to avoid all at once
        setTimeout(() => {
          this.launchHeadless(config.id, state).catch((err) => {
            this.addLog(config.id, `Restore failed: ${err}`);
          });
        }, 500);
      }
    }
  }

  killAll(): void {
    this.connections.forEach((state, id) => {
      this.killProcess(id, state);
    });
  }

  // ── Internal ────────────────────────────────────────────────

  private toConnectionInfo(state: ConnectionState): ConnectionInfo {
    return {
      id: state.config.id,
      alias: state.config.alias,
      platform: state.config.platform,
      status: state.status,
      joinLink: state.config.joinLink,
      currentJoinLink: state.currentJoinLink,
      turn: state.turn,
      protocol: state.protocol,
      tunnelConnected: state.tunnelConnected,
      autoRestart: state.config.autoRestart,
      createdAt: state.config.createdAt,
      error: state.error,
      clientConnected: state.clientConnected,
      activeConns: state.activeConns,
      recvMB: state.recvMB,
      sendMB: state.sendMB,
    };
  }

  private emitUpdate(id: string): void {
    const state = this.connections.get(id);
    if (!state) return;
    this.onEvent('update', id, this.toConnectionInfo(state));
  }

  private addLog(id: string, msg: string): void {
    const state = this.connections.get(id);
    if (!state) return;
    state.logs.push(msg);
    if (state.logs.length > MAX_LOG_LINES) {
      state.logs = state.logs.slice(-MAX_LOG_LINES);
    }
    this.onEvent('log', id, msg);
  }

  private async isPortFree(port: number): Promise<boolean> {
    return new Promise((resolve) => {
      const server = net.createServer();
      server.once('error', () => resolve(false));
      server.once('listening', () => {
        server.close(() => resolve(true));
      });
      server.listen(port, '127.0.0.1');
    });
  }

  private async allocPorts(): Promise<{ dc: number; pion: number }> {
    while (true) {
      const dc = this.nextPortBase;
      const pion = this.nextPortBase + 1;
      this.nextPortBase += 2;
      if (await this.isPortFree(dc) && await this.isPortFree(pion)) {
        return { dc, pion };
      }
    }
  }

  private appendUpstreamArgs(args: string[]): void {
    if (!this.upstreamProxy.socks) return;
    args.push('--upstream-socks', this.upstreamProxy.socks);
    if (this.upstreamProxy.user) args.push('--upstream-user', this.upstreamProxy.user);
    if (this.upstreamProxy.pass) args.push('--upstream-pass', this.upstreamProxy.pass);
  }

  private killProcess(id: string, state: ConnectionState): void {
    if (state.process) {
      console.log(`[CM] Killing process for ${id} pid=${state.process.pid}`);
      state.process.kill();
      state.process = null;
    }
  }

  private async launchHeadless(id: string, state: ConnectionState): Promise<void> {
    this.killProcess(id, state);

    const platform = state.config.platform;
    const binaryBase = PLATFORM_BINARY[platform];
    if (!binaryBase) {
      state.status = ConnectionStatus.Error;
      state.error = `Unknown platform: ${platform}`;
      this.emitUpdate(id);
      return;
    }

    const binaryPath = resolveBinary(this.binsDir, binaryBase);
    const cookiesPath = this.store.cookieFilePath(platform);

    if (!fs.existsSync(cookiesPath)) {
      state.status = ConnectionStatus.Error;
      state.error = `No cookies for ${platform}. Upload cookies first.`;
      this.addLog(id, state.error);
      this.emitUpdate(id);
      return;
    }

    state.status = ConnectionStatus.Starting;
    state.error = undefined;
    state.tunnelConnected = false;
    state.currentJoinLink = undefined;
    state.turn = undefined;
    state.protocol = undefined;
    state.clientConnected = false;
    state.activeConns = 0;
    state.recvMB = 0;
    state.sendMB = 0;
    this.emitUpdate(id);

    const spawnArgs = ['--resources', this.resources, '--cookies', cookiesPath];

    // Determine join target: saved joinLink for "join" mode, or
    // currentJoinLink from a previous run (auto-reconnect after crash)
    const joinTarget = state.currentJoinLink || state.config.joinLink;
    if (joinTarget) {
      const flag = PLATFORM_JOIN_FLAG[platform];
      if (flag) spawnArgs.push(flag, joinTarget);
    }

    this.appendUpstreamArgs(spawnArgs);
    if (this.debugLogging) spawnArgs.push('--debug');

    this.addLog(id, `Starting ${binaryPath} ${spawnArgs.join(' ')}`);

    let proc: ChildProcess;
    try {
      proc = spawn(binaryPath, spawnArgs, {
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (err) {
      state.status = ConnectionStatus.Error;
      state.error = `Failed to spawn: ${err}`;
      this.addLog(id, state.error);
      this.emitUpdate(id);
      return;
    }

    state.process = proc;
    state.status = ConnectionStatus.Active;
    this.emitUpdate(id);

    let sawAuthFailure = false;

    const onData = (data: Buffer) => {
      data
        .toString()
        .trim()
        .split('\n')
        .forEach((msg) => {
          if (!msg) return;
          // Don't log stats lines to UI (they repeat every 5s)
          if (!msg.trimStart().startsWith('STATS: ')) {
            this.addLog(id, msg);
          }
          this.parseHeadlessLog(id, state, msg);

          if (
            msg.includes('status 401') ||
            msg.includes('"UnauthorizedError"') ||
            msg.includes('"error":"unauthorized') ||
            msg.includes('empty access_token')
          ) {
            sawAuthFailure = true;
          }
        });
    };

    proc.stdout?.on('data', onData);
    proc.stderr?.on('data', onData);

    proc.on('close', (code) => {
      this.addLog(id, `Headless exited with code ${code}`);

      if (sawAuthFailure) {
        state.status = ConnectionStatus.Error;
        state.error = `Auth failed — cookies expired. Re-upload cookies for ${platform}.`;
        state.process = null;
        this.emitUpdate(id);
        return;
      }

      // If process reference still matches, it was an unexpected crash
      if (state.process !== proc || !this.connections.has(id)) return;
      state.process = null;

      if (state.config.autoRestart) {
        this.restartAfterCrash(id, state, code);
      } else {
        state.status = ConnectionStatus.Stopped;
        this.emitUpdate(id);
      }
    });
  }

  private parseHeadlessLog(id: string, state: ConnectionState, msg: string): void {
    const trimmed = msg.trim();

    if (trimmed === HeadlessLogMarker.CALL_CREATED) {
      state.status = ConnectionStatus.Active;
      this.emitUpdate(id);
    }

    if (trimmed.includes(HeadlessLogMarker.JOIN_LINK)) {
      state.currentJoinLink = trimmed.split(HeadlessLogMarker.JOIN_LINK)[1].trim();
      // Persist the join link so we can restore after app restart
      this.store.updateConnection(id, { joinLink: state.currentJoinLink });
      this.emitUpdate(id);
    }

    if (trimmed.includes(HeadlessLogMarker.TURN)) {
      state.turn = trimmed.split(HeadlessLogMarker.TURN)[1].trim();
      this.emitUpdate(id);
    }

    if (trimmed.includes(HeadlessLogMarker.PROTOCOL)) {
      state.protocol = trimmed.split(HeadlessLogMarker.PROTOCOL)[1].trim();
      this.emitUpdate(id);
    }

    if (trimmed.includes(HeadlessLogMarker.TUNNEL_CONNECTED)) {
      state.tunnelConnected = true;
      state.status = ConnectionStatus.Connected;
      state.crashCount = 0;
      this.emitUpdate(id);
    }

    if (trimmed.includes('[FATAL]')) {
      const fatalMsg = trimmed.split('[FATAL]')[1]?.trim() || 'fatal error';
      state.error = fatalMsg;
      state.tunnelConnected = false;
      this.emitUpdate(id);
    }

    // Parse stats output (not logged to avoid spam)
    if (trimmed.startsWith(HeadlessLogMarker.STATS)) {
      try {
        const json = JSON.parse(trimmed.slice(HeadlessLogMarker.STATS.length));
        state.clientConnected = (json.tcpConns + json.udpConns) > 0;
        state.activeConns = json.tcpConns + json.udpConns;
        state.recvMB = json.recvBytes / 1048576;
        state.sendMB = json.sendBytes / 1048576;
        this.emitUpdate(id);
      } catch { /* malformed stats line, ignore */ }
    }
  }

  private restartAfterCrash(id: string, state: ConnectionState, code: number | null): void {
    const attempt = (state.crashCount || 0) + 1;
    state.crashCount = attempt;

    if (attempt > MAX_CRASH_RESTARTS) {
      state.status = ConnectionStatus.Error;
      state.error = `Crashed ${attempt} times in a row (last code ${code}). Not restarting. Start manually.`;
      this.emitUpdate(id);
      return;
    }

    const delay = RESTART_DELAY_MS * attempt;
    const how = state.currentJoinLink ? `rejoining ${state.currentJoinLink}` : 'creating a new call';

    state.status = ConnectionStatus.Reconnecting;
    this.addLog(id, `Crashed (code ${code}), ${how} in ${Math.round(delay / 1000)}s (attempt ${attempt}/${MAX_CRASH_RESTARTS}).`);
    this.emitUpdate(id);

    setTimeout(() => {
      if (!this.connections.has(id) || state.process) return;
      this.launchHeadless(id, state).catch((err) => {
        this.addLog(id, `Restart failed: ${err}`);
      });
    }, delay);
  }
}
