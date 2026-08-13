import 'dotenv/config';
import * as http from 'http';
import * as path from 'path';
import express from 'express';
import multer from 'multer';
import * as QRCode from 'qrcode';
import { WebSocketServer, WebSocket } from 'ws';
import { authMiddleware } from './auth';
import { Store } from './store';
import { ConnectionManager } from './connection-manager';
import { Platform, WSMessage } from './types';
import { PLATFORM_LABELS } from './constants';

const PORT = parseInt(process.env.WEB_PORT || '3000', 10);
const app = express();
const server = http.createServer(app);
const store = new Store();
const cm = new ConnectionManager(store);
const upload = multer({ storage: multer.memoryStorage(), limits: { fileSize: 5 * 1024 * 1024 } });

// ── WebSocket ─────────────────────────────────────────────────

const wss = new WebSocketServer({ server, path: '/ws' });
const wsClients = new Set<WebSocket>();

wss.on('connection', (ws) => {
  wsClients.add(ws);
  // Send initial state
  const msg: WSMessage = { type: 'connections-list', data: cm.getAllConnections() };
  ws.send(JSON.stringify(msg));
  const cookiesMsg: WSMessage = { type: 'cookies-update', data: store.listCookies() };
  ws.send(JSON.stringify(cookiesMsg));
  ws.on('close', () => wsClients.delete(ws));
});

function broadcast(msg: WSMessage): void {
  const payload = JSON.stringify(msg);
  wsClients.forEach((ws) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(payload);
  });
}

cm.setEventCallback((type, connectionId, data) => {
  if (type === 'update') {
    broadcast({ type: 'connection-update', connectionId, data });
  } else if (type === 'log') {
    broadcast({ type: 'log', connectionId, data });
  }
});

// ── Middleware ─────────────────────────────────────────────────

app.use(authMiddleware);
app.use(express.json());
app.use(express.static(path.join(__dirname, '..', 'public')));

// ── REST API: Connections ─────────────────────────────────────

app.get('/api/connections', (_req, res) => {
  res.json(cm.getAllConnections());
});

app.post('/api/connections', async (req, res) => {
  const { alias, platform, joinLink, autoRestart } = req.body;
  if (!alias || !platform) {
    res.status(400).json({ error: 'alias and platform are required' });
    return;
  }
  if (!Object.values(Platform).includes(platform)) {
    res.status(400).json({ error: `Invalid platform. Use: ${Object.values(Platform).join(', ')}` });
    return;
  }
  try {
    const info = await cm.createConnection(alias, platform, joinLink, autoRestart !== false);
    res.status(201).json(info);
  } catch (err) {
    res.status(500).json({ error: String(err) });
  }
});

app.get('/api/connections/:id', (req, res) => {
  const info = cm.getConnection(req.params.id);
  if (!info) { res.status(404).json({ error: 'Not found' }); return; }
  res.json(info);
});

app.put('/api/connections/:id', (req, res) => {
  const { alias, autoRestart, joinLink } = req.body;
  const info = cm.updateConnection(req.params.id, { alias, autoRestart, joinLink });
  if (!info) { res.status(404).json({ error: 'Not found' }); return; }
  res.json(info);
});

app.delete('/api/connections/:id', async (req, res) => {
  const ok = await cm.deleteConnection(req.params.id);
  if (!ok) { res.status(404).json({ error: 'Not found' }); return; }
  broadcast({ type: 'connections-list', data: cm.getAllConnections() });
  res.json({ success: true });
});

app.post('/api/connections/:id/start', async (req, res) => {
  const ok = await cm.startConnection(req.params.id);
  if (!ok) { res.status(404).json({ error: 'Not found' }); return; }
  res.json({ success: true });
});

app.post('/api/connections/:id/stop', (req, res) => {
  const ok = cm.stopConnection(req.params.id);
  if (!ok) { res.status(404).json({ error: 'Not found' }); return; }
  res.json({ success: true });
});

app.get('/api/connections/:id/logs', (req, res) => {
  const logs = cm.getLogs(req.params.id);
  res.json(logs);
});

app.get('/api/connections/:id/qr', async (req, res) => {
  const info = cm.getConnection(req.params.id);
  if (!info || !info.currentJoinLink) {
    res.status(404).json({ error: 'No join link available' });
    return;
  }
  try {
    const dataUrl = await QRCode.toDataURL(info.currentJoinLink, {
      errorCorrectionLevel: 'M',
      margin: 1,
      scale: 6,
    });
    res.json({ qr: dataUrl, link: info.currentJoinLink });
  } catch {
    res.status(500).json({ error: 'QR generation failed' });
  }
});

// ── REST API: Cookies ─────────────────────────────────────────

app.get('/api/cookies', (_req, res) => {
  res.json(store.listCookies());
});

app.post('/api/cookies/:platform', upload.single('file'), (req, res) => {
  const platform = req.params.platform;
  if (!['vk', 'telemost', 'wbstream', 'dion'].includes(platform)) {
    res.status(400).json({ error: 'Invalid platform' });
    return;
  }
  if (!req.file) {
    res.status(400).json({ error: 'No file uploaded' });
    return;
  }
  // Validate JSON
  try {
    JSON.parse(req.file.buffer.toString('utf8'));
  } catch {
    res.status(400).json({ error: 'Invalid JSON file' });
    return;
  }
  store.saveCookieFile(platform, req.file.buffer);
  broadcast({ type: 'cookies-update', data: store.listCookies() });
  res.json({ success: true, platform });
});

app.delete('/api/cookies/:platform', (req, res) => {
  const ok = store.deleteCookieFile(req.params.platform);
  broadcast({ type: 'cookies-update', data: store.listCookies() });
  res.json({ success: ok });
});

// ── REST API: Settings ────────────────────────────────────────

app.get('/api/settings', (_req, res) => {
  res.json(store.loadSettings());
});

app.put('/api/settings', (req, res) => {
  const settings = { ...store.loadSettings(), ...req.body };
  store.saveSettings(settings);
  if (settings.upstreamProxy) cm.setUpstreamProxy(settings.upstreamProxy);
  if (settings.debugLogging !== undefined) cm.setDebugLogging(settings.debugLogging);
  if (settings.resources) cm.setResources(settings.resources);
  res.json(settings);
});

// ── REST API: Platforms metadata ──────────────────────────────

app.get('/api/platforms', (_req, res) => {
  res.json(
    Object.entries(PLATFORM_LABELS).map(([value, label]) => ({ value, label })),
  );
});

// ── SPA fallback ──────────────────────────────────────────────

app.get('*', (_req, res) => {
  res.sendFile(path.join(__dirname, '..', 'public', 'index.html'));
});

// ── Startup ───────────────────────────────────────────────────

async function start(): Promise<void> {
  await cm.restoreConnections();
  server.listen(PORT, () => {
    console.log(`\n  ┌──────────────────────────────────────────┐`);
    console.log(`  │  Creator Web Panel                       │`);
    console.log(`  │  http://localhost:${PORT}                    │`);
    console.log(`  └──────────────────────────────────────────┘\n`);
  });
}

// ── Graceful shutdown ─────────────────────────────────────────

function shutdown(): void {
  console.log('\n[server] Shutting down...');
  cm.killAll();
  server.close();
  process.exit(0);
}

process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
process.on('exit', () => cm.killAll());

start().catch((err) => {
  console.error('[server] Fatal:', err);
  process.exit(1);
});
