export const INITIAL_PORT_BASE = 10000;
export const MAX_CRASH_RESTARTS = 5;
export const RESTART_DELAY_MS = 500;
export const MAX_LOG_LINES = 2000;

export const USER_AGENT =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) ' +
  'AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36';

export enum HeadlessLogMarker {
  CALL_CREATED = 'CALL CREATED',
  JOIN_LINK = 'join_link:',
  TURN = 'TURN:',
  PROTOCOL = 'protocol:',
  TUNNEL_CONNECTED = 'TUNNEL CONNECTED',
  STATS = 'STATS: ',
}

export const PLATFORM_JOIN_FLAG: Record<string, string> = {
  vk: '--vk-link',
  telemost: '--tm-link',
  wbstream: '--room',
  dion: '--room',
};

export const PLATFORM_BINARY: Record<string, string> = {
  vk: 'headless-vk-creator',
  telemost: 'headless-telemost-creator',
  wbstream: 'headless-wbstream-creator',
  dion: 'headless-dion-creator',
};

export const PLATFORM_COOKIE_FILE: Record<string, string> = {
  vk: 'cookies-vk.json',
  telemost: 'cookies-yandex.json',
  wbstream: 'cookies-wbstream.json',
  dion: 'cookies-dion.json',
};

export const PLATFORM_LABELS: Record<string, string> = {
  vk: 'VK',
  telemost: 'Telemost',
  wbstream: 'WB Stream',
  dion: 'DION',
};
