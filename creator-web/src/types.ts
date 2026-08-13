import type { ChildProcess } from 'child_process';

export enum Platform {
  VK = 'vk',
  Telemost = 'telemost',
  WBStream = 'wbstream',
  Dion = 'dion',
}

export enum HeadlessMode {
  Create = 'create',
  Join = 'join',
}

export enum ConnectionStatus {
  Saved = 'saved',
  Starting = 'starting',
  Active = 'active',
  Connected = 'connected',
  Reconnecting = 'reconnecting',
  Error = 'error',
  Stopped = 'stopped',
}

export interface ConnectionConfig {
  id: string;
  alias: string;
  platform: Platform;
  joinLink?: string;
  autoRestart: boolean;
  createdAt: string;
}

export interface ConnectionState {
  config: ConnectionConfig;
  status: ConnectionStatus;
  process: ChildProcess | null;
  currentJoinLink?: string;
  turn?: string;
  protocol?: string;
  tunnelConnected: boolean;
  crashCount: number;
  logs: string[];
  dcPort: number;
  pionPort: number;
  error?: string;
}

export interface CookieEntry {
  name: string;
  value: string;
}

export interface CookieFileContent {
  email?: string;
  password?: string;
  cookies: CookieEntry[];
}

export interface DionCredentials {
  email: string;
  password: string;
}

export interface UpstreamProxy {
  socks: string;
  user: string;
  pass: string;
}

export interface AppSettings {
  upstreamProxy: UpstreamProxy;
  debugLogging: boolean;
  resources: string;
  vkBot: {
    token: string;
    groupId: string;
    userIds: string;
  };
}

/** Serialised representation sent to UI via REST / WebSocket */
export interface ConnectionInfo {
  id: string;
  alias: string;
  platform: Platform;
  status: ConnectionStatus;
  joinLink?: string;
  currentJoinLink?: string;
  turn?: string;
  protocol?: string;
  tunnelConnected: boolean;
  autoRestart: boolean;
  createdAt: string;
  error?: string;
}

/** WebSocket message from server → client */
export interface WSMessage {
  type: 'connection-update' | 'log' | 'connections-list' | 'cookies-update' | 'error';
  connectionId?: string;
  data: unknown;
}
