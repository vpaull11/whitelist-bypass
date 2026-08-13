/* ═══════════════════════════════════════════════════════════════
   Creator Web Panel — Client Application
   ═══════════════════════════════════════════════════════════════ */

const PLATFORM_LABELS = { vk: 'VK', telemost: 'Telemost', wbstream: 'WB Stream', dion: 'DION' };
const STATUS_LABELS = {
  saved: 'Saved', starting: 'Starting...', active: 'Active',
  connected: 'Connected', reconnecting: 'Reconnecting...', error: 'Error', stopped: 'Stopped',
};
const STATUS_DOT_CLASS = {
  saved: 'status-dot--stopped', starting: 'status-dot--starting', active: 'status-dot--online',
  connected: 'status-dot--online', reconnecting: 'status-dot--reconnecting',
  error: 'status-dot--error', stopped: 'status-dot--stopped',
};
const STATUS_TEXT_CLASS = {
  connected: 'card-status-text--connected', error: 'card-status-text--error',
  starting: 'card-status-text--starting', reconnecting: 'card-status-text--reconnecting',
};

// ── State ─────────────────────────────────────────────────────

let connections = [];
let cookies = {};
let currentLogId = null;
let ws = null;
let authenticated = false;

// ── Helpers ───────────────────────────────────────────────────

function $(id) { return document.getElementById(id); }
function esc(s) { const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }

async function api(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (body) opts.body = JSON.stringify(body);
  const res = await fetch('/api' + path, opts);
  if (res.status === 401) {
    showLoginScreen();
    throw new Error('Session expired. Please log in again.');
  }
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || 'Request failed');
  return data;
}

function toast(msg, type = 'info') {
  const el = document.createElement('div');
  el.className = `toast toast--${type}`;
  el.textContent = msg;
  $('toastContainer').appendChild(el);
  setTimeout(() => { el.style.opacity = '0'; setTimeout(() => el.remove(), 300); }, 4000);
}

async function copyText(text) {
  try { await navigator.clipboard.writeText(text); toast('Copied to clipboard', 'success'); }
  catch { toast('Failed to copy', 'error'); }
}

function formatMB(mb) {
  if (mb < 0.01) return '0 B';
  if (mb < 1) return (mb * 1024).toFixed(0) + ' KB';
  if (mb < 1024) return mb.toFixed(1) + ' MB';
  return (mb / 1024).toFixed(2) + ' GB';
}

// ── Auth ──────────────────────────────────────────────────────

function showLoginScreen() {
  authenticated = false;
  $('loginScreen').classList.remove('login-screen--hidden');
  $('appContainer').classList.add('app-container--hidden');
  $('loginUser').focus();
  if (ws) { ws.close(); ws = null; }
}

function showApp() {
  authenticated = true;
  $('loginScreen').classList.add('login-screen--hidden');
  $('appContainer').classList.remove('app-container--hidden');
  connectWS();
  fetchInitialData();
}

async function checkAuth() {
  try {
    const res = await fetch('/api/auth/check');
    const data = await res.json();
    if (data.authenticated) {
      showApp();
    } else {
      showLoginScreen();
    }
  } catch {
    showLoginScreen();
  }
}

async function handleLogin(e) {
  e.preventDefault();
  const user = $('loginUser').value.trim();
  const pass = $('loginPass').value;
  $('loginError').textContent = '';

  if (!user || !pass) {
    $('loginError').textContent = 'Enter username and password';
    return;
  }

  try {
    const res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user, pass }),
    });
    const data = await res.json();
    if (!res.ok) {
      $('loginError').textContent = data.error || 'Login failed';
      return;
    }
    $('loginPass').value = '';
    showApp();
  } catch (err) {
    $('loginError').textContent = 'Connection error';
  }
}

async function handleLogout() {
  try {
    await fetch('/api/auth/logout', { method: 'POST' });
  } catch { /* ignore */ }
  showLoginScreen();
}

// ── WebSocket ─────────────────────────────────────────────────

function connectWS() {
  if (!authenticated) return;
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(`${proto}//${location.host}/ws`);

  ws.onmessage = (event) => {
    const msg = JSON.parse(event.data);
    switch (msg.type) {
      case 'connections-list':
        connections = msg.data;
        renderConnections();
        break;
      case 'connection-update':
        updateConnectionInList(msg.data);
        renderConnections();
        break;
      case 'log':
        if (currentLogId === msg.connectionId) appendLog(msg.data);
        break;
      case 'cookies-update':
        cookies = msg.data;
        renderCookies();
        break;
    }
  };

  ws.onclose = () => {
    $('serverStatus').innerHTML = '<span class="status-dot status-dot--error"></span><span>Disconnected</span>';
    if (authenticated) setTimeout(connectWS, 3000);
  };

  ws.onopen = () => {
    $('serverStatus').innerHTML = '<span class="status-dot status-dot--online"></span><span>Online</span>';
  };
}

function updateConnectionInList(updated) {
  const idx = connections.findIndex(c => c.id === updated.id);
  if (idx >= 0) connections[idx] = updated;
  else connections.push(updated);
}

// ── Render: Connections ───────────────────────────────────────

function renderConnections() {
  const grid = $('connectionsGrid');
  const empty = $('emptyConnections');

  if (connections.length === 0) {
    grid.innerHTML = '';
    empty.classList.remove('empty-state--hidden');
    return;
  }
  empty.classList.add('empty-state--hidden');

  grid.innerHTML = connections.map(conn => {
    const statusDot = STATUS_DOT_CLASS[conn.status] || 'status-dot--stopped';
    const statusLabel = STATUS_LABELS[conn.status] || conn.status;
    const statusClass = STATUS_TEXT_CLASS[conn.status] || '';
    const platformLabel = PLATFORM_LABELS[conn.platform] || conn.platform;

    let linkHtml = '';
    if (conn.currentJoinLink) {
      linkHtml = `
        <div class="card-link" onclick="copyText('${esc(conn.currentJoinLink)}')" title="Click to copy">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>
          <span class="card-link-text">${esc(conn.currentJoinLink)}</span>
        </div>`;
    }

    let errorHtml = '';
    if (conn.error && conn.status === 'error') {
      errorHtml = `<div class="card-error">${esc(conn.error)}</div>`;
    }

    const isRunning = ['starting', 'active', 'connected', 'reconnecting'].includes(conn.status);

    // Stats block (only when running)
    let statsHtml = '';
    if (isRunning || conn.status === 'connected') {
      const clientClass = conn.clientConnected ? 'card-stats-client--yes' : 'card-stats-client--no';
      const clientLabel = conn.clientConnected
        ? `👤 Client (${conn.activeConns || 0})`
        : '👤 No client';
      statsHtml = `
        <div class="card-stats">
          <div class="card-stats-client ${clientClass}">${clientLabel}</div>
          <div class="card-stats-traffic">
            <span class="card-stats-item"><span class="card-stats-arrow card-stats-arrow--down">↓</span> ${formatMB(conn.recvMB || 0)}</span>
            <span class="card-stats-item"><span class="card-stats-arrow card-stats-arrow--up">↑</span> ${formatMB(conn.sendMB || 0)}</span>
          </div>
        </div>`;
    }

    const startBtn = !isRunning
      ? `<button class="btn btn--success btn--sm" onclick="startConn('${conn.id}')">▶ Start</button>`
      : `<button class="btn btn--sm" onclick="stopConn('${conn.id}')">⏹ Stop</button>`;

    return `
      <div class="card" data-platform="${conn.platform}" data-id="${conn.id}">
        <div class="card-header">
          <div class="card-title">${esc(conn.alias)}</div>
          <div class="card-platform">${platformLabel}</div>
        </div>
        <div class="card-status">
          <span class="status-dot ${statusDot}"></span>
          <span class="${statusClass}">${statusLabel}</span>
          ${conn.tunnelConnected ? '<span style="color:var(--success);margin-left:4px">● tunnel</span>' : ''}
        </div>
        ${linkHtml}
        ${errorHtml}
        ${statsHtml}
        <div class="card-actions">
          ${startBtn}
          <button class="btn btn--sm" onclick="showLogs('${conn.id}', '${esc(conn.alias)}')">📋 Logs</button>
          ${conn.currentJoinLink ? `<button class="btn btn--sm" onclick="showQR('${conn.id}')">QR</button>` : ''}
          <button class="btn btn--danger btn--sm" onclick="deleteConn('${conn.id}')">✕</button>
        </div>
      </div>`;
  }).join('');
}

// ── Render: Cookies ───────────────────────────────────────────

function renderCookies() {
  const grid = $('cookiesGrid');
  const platforms = ['vk', 'telemost', 'wbstream', 'dion'];

  grid.innerHTML = platforms.map(p => {
    const info = cookies[p] || { exists: false, size: 0 };
    const label = PLATFORM_LABELS[p] || p;
    const badge = info.exists
      ? '<span class="cookie-card-badge cookie-card-badge--ok">OK</span>'
      : '<span class="cookie-card-badge cookie-card-badge--missing">Missing</span>';
    const infoText = info.exists
      ? `${(info.size / 1024).toFixed(1)} KB · ${new Date(info.modifiedAt).toLocaleString()}`
      : 'No cookies uploaded';

    return `
      <div class="cookie-card">
        <div class="cookie-card-header">
          <div class="cookie-card-title">${label}</div>
          ${badge}
        </div>
        <div class="cookie-card-info">${infoText}</div>
        <div class="cookie-card-actions">
          <button class="btn btn--sm btn--primary" onclick="uploadCookie('${p}')">Upload</button>
          ${info.exists ? `<button class="btn btn--sm btn--danger" onclick="deleteCookie('${p}')">Delete</button>` : ''}
        </div>
        <input type="file" class="cookie-upload-input" id="cookieInput_${p}" accept=".json" onchange="handleCookieUpload('${p}', this)">
      </div>`;
  }).join('');
}

// ── Actions ───────────────────────────────────────────────────

async function startConn(id) {
  try { await api('POST', `/connections/${id}/start`); toast('Started', 'success'); }
  catch (e) { toast(e.message, 'error'); }
}

async function stopConn(id) {
  try { await api('POST', `/connections/${id}/stop`); toast('Stopped', 'info'); }
  catch (e) { toast(e.message, 'error'); }
}

async function deleteConn(id) {
  if (!confirm('Delete this connection?')) return;
  try { await api('DELETE', `/connections/${id}`); toast('Deleted', 'info'); }
  catch (e) { toast(e.message, 'error'); }
}

async function createConnection() {
  const alias = $('newAlias').value.trim();
  const platform = $('newPlatform').value;
  const joinLink = $('newJoinLink').value.trim();
  const autoRestart = $('newAutoRestart').checked;
  const autoStart = $('newAutoStart').checked;

  if (!alias) { toast('Alias is required', 'error'); return; }

  try {
    const conn = await api('POST', '/connections', { alias, platform, joinLink: joinLink || undefined, autoRestart });
    closeModal('modalNewConnection');
    toast(`Created "${alias}"`, 'success');
    if (autoStart) {
      await api('POST', `/connections/${conn.id}/start`);
    }
    // Reset form
    $('newAlias').value = '';
    $('newJoinLink').value = '';
  } catch (e) {
    toast(e.message, 'error');
  }
}

function uploadCookie(platform) {
  $('cookieInput_' + platform).click();
}

async function handleCookieUpload(platform, input) {
  const file = input.files[0];
  if (!file) return;
  const formData = new FormData();
  formData.append('file', file);
  try {
    const res = await fetch(`/api/cookies/${platform}`, { method: 'POST', body: formData });
    if (res.status === 401) { showLoginScreen(); return; }
    const data = await res.json();
    if (!res.ok) throw new Error(data.error);
    toast(`${PLATFORM_LABELS[platform]} cookies uploaded`, 'success');
  } catch (e) {
    toast(e.message, 'error');
  }
  input.value = '';
}

async function deleteCookie(platform) {
  if (!confirm(`Delete ${PLATFORM_LABELS[platform]} cookies?`)) return;
  try { await api('DELETE', `/cookies/${platform}`); toast('Deleted', 'info'); }
  catch (e) { toast(e.message, 'error'); }
}

// ── Logs Modal ────────────────────────────────────────────────

async function showLogs(id, alias) {
  currentLogId = id;
  $('logsTitle').textContent = `Logs — ${alias}`;
  $('logView').textContent = 'Loading...';
  openModal('modalLogs');
  try {
    const logs = await api('GET', `/connections/${id}/logs`);
    $('logView').textContent = logs.join('\n') || '(empty)';
    scrollLogView();
  } catch (e) {
    $('logView').textContent = 'Failed to load logs: ' + e.message;
  }
}

function appendLog(msg) {
  const el = $('logView');
  if (!el) return;
  if (el.textContent === '(empty)') el.textContent = '';
  el.textContent += (el.textContent ? '\n' : '') + msg;
  scrollLogView();
}

function scrollLogView() {
  const el = $('logView');
  if (el) el.scrollTop = el.scrollHeight;
}

// ── QR Modal ──────────────────────────────────────────────────

async function showQR(id) {
  try {
    const data = await api('GET', `/connections/${id}/qr`);
    const el = document.createElement('div');
    el.innerHTML = `
      <div style="text-align:center;padding:20px">
        <img src="${data.qr}" alt="QR" style="max-width:260px;border-radius:8px;background:white;padding:8px">
        <p style="margin-top:12px;font-size:0.82rem;color:var(--text-secondary);word-break:break-all">${esc(data.link)}</p>
      </div>`;
    // Reuse logs modal for simplicity
    $('logsTitle').textContent = 'QR Code';
    $('logView').replaceWith(el.firstElementChild);
    openModal('modalLogs');
    // Restore logView on close
    const original = $('logView');
    if (!original) {
      const pre = document.createElement('pre');
      pre.className = 'log-view';
      pre.id = 'logView';
      $('modalLogs').querySelector('.modal-body').appendChild(pre);
    }
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Settings ──────────────────────────────────────────────────

async function loadSettings() {
  try {
    const s = await api('GET', '/settings');
    $('settingsSocks').value = s.upstreamProxy?.socks || '';
    $('settingsProxyUser').value = s.upstreamProxy?.user || '';
    $('settingsProxyPass').value = s.upstreamProxy?.pass || '';
    $('settingsResources').value = s.resources || 'default';
    $('settingsDebug').checked = !!s.debugLogging;
    $('settingsBotToken').value = s.vkBot?.token || '';
    $('settingsBotGroupId').value = s.vkBot?.groupId || '';
    $('settingsBotUserIds').value = s.vkBot?.userIds || '';
  } catch { /* ignore */ }
}

async function saveSettings() {
  const settings = {
    upstreamProxy: {
      socks: $('settingsSocks').value.trim(),
      user: $('settingsProxyUser').value.trim(),
      pass: $('settingsProxyPass').value,
    },
    resources: $('settingsResources').value,
    debugLogging: $('settingsDebug').checked,
    vkBot: {
      token: $('settingsBotToken').value.trim(),
      groupId: $('settingsBotGroupId').value.trim(),
      userIds: $('settingsBotUserIds').value.trim(),
    },
  };
  try {
    await api('PUT', '/settings', settings);
    toast('Settings saved', 'success');
  } catch (e) {
    toast(e.message, 'error');
  }
}

// ── Navigation ────────────────────────────────────────────────

function switchSection(name) {
  document.querySelectorAll('.section').forEach(s => s.classList.add('section--hidden'));
  document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('nav-btn--active'));
  $('section' + name.charAt(0).toUpperCase() + name.slice(1)).classList.remove('section--hidden');
  document.querySelector(`[data-section="${name}"]`).classList.add('nav-btn--active');
}

// ── Modals ────────────────────────────────────────────────────

function openModal(id) {
  $(id).classList.add('modal-overlay--visible');
}

function closeModal(id) {
  $(id).classList.remove('modal-overlay--visible');
  if (id === 'modalLogs') {
    currentLogId = null;
    // Ensure logView pre exists
    const body = $(id).querySelector('.modal-body');
    if (!$('logView')) {
      const pre = document.createElement('pre');
      pre.className = 'log-view';
      pre.id = 'logView';
      body.innerHTML = '';
      body.appendChild(pre);
    }
  }
}

// ── Init ──────────────────────────────────────────────────────

function init() {
  // Login form
  $('loginForm').addEventListener('submit', handleLogin);

  // Logout
  $('btnLogout').addEventListener('click', handleLogout);

  // Navigation
  $('nav').addEventListener('click', (e) => {
    const btn = e.target.closest('.nav-btn');
    if (!btn) return;
    switchSection(btn.dataset.section);
    if (btn.dataset.section === 'settings') loadSettings();
  });

  // New connection
  $('btnNewConnection').addEventListener('click', () => openModal('modalNewConnection'));
  $('btnCreateConnection').addEventListener('click', createConnection);

  // Settings
  $('btnSaveSettings').addEventListener('click', saveSettings);

  // Logs
  $('btnClearLogs').addEventListener('click', () => { $('logView').textContent = ''; });

  // Modal close buttons
  document.querySelectorAll('[data-close]').forEach(btn => {
    btn.addEventListener('click', () => {
      const modal = btn.closest('.modal-overlay');
      if (modal) closeModal(modal.id);
    });
  });

  // Modal overlay click to close
  document.querySelectorAll('.modal-overlay').forEach(overlay => {
    overlay.addEventListener('click', (e) => {
      if (e.target === overlay) closeModal(overlay.id);
    });
  });

  // Keyboard shortcut: Escape to close modals
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      document.querySelectorAll('.modal-overlay--visible').forEach(m => closeModal(m.id));
    }
  });

  // Enter key in new connection form
  $('newAlias').addEventListener('keydown', (e) => { if (e.key === 'Enter') createConnection(); });
  $('newJoinLink').addEventListener('keydown', (e) => { if (e.key === 'Enter') createConnection(); });

  // Check auth on startup
  checkAuth();
}

async function fetchInitialData() {
  try {
    connections = await api('GET', '/connections');
    renderConnections();
  } catch { /* WS will catch up */ }
  try {
    cookies = await api('GET', '/cookies');
    renderCookies();
  } catch { /* ignore */ }
}

document.addEventListener('DOMContentLoaded', init);

