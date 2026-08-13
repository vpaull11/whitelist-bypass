import * as crypto from 'crypto';
import type { Request, Response, NextFunction } from 'express';

/** Active session tokens (in-memory, cleared on restart — users re-login). */
const sessions = new Set<string>();

/** Check if a token is a valid active session. */
export function isValidSession(token: string): boolean {
  return sessions.has(token);
}

/** Generate a cryptographically secure session token. */
export function createSession(): string {
  const token = crypto.randomBytes(32).toString('hex');
  sessions.add(token);
  return token;
}

/** Destroy a session. */
export function destroySession(token: string): void {
  sessions.delete(token);
}

/** Validate login credentials against env config. */
export function validateCredentials(user: string, pass: string): boolean {
  const envUser = process.env.WEB_USER;
  const envPass = process.env.WEB_PASS;
  if (!envUser || !envPass) return false;
  return user === envUser && pass === envPass;
}

/** Extract session token from cookie header or Authorization header. */
function extractToken(req: Request): string | null {
  // 1. Check cookie
  const cookieHeader = req.headers.cookie || '';
  const match = cookieHeader.match(/(?:^|;\s*)session=([a-f0-9]+)/);
  if (match) return match[1];

  // 2. Check Authorization: Bearer <token>
  const auth = req.headers.authorization;
  if (auth && auth.startsWith('Bearer ')) return auth.slice(7);

  return null;
}

/**
 * Session auth middleware.
 * Allows: POST /api/auth/login, static assets (login page).
 * Blocks everything else without a valid session token.
 */
export function authMiddleware(req: Request, res: Response, next: NextFunction): void {
  // Always allow the login endpoint
  if (req.path === '/api/auth/login' && req.method === 'POST') {
    next();
    return;
  }

  // Always allow static assets so the login page can render
  if (
    req.path === '/' ||
    req.path === '/index.html' ||
    req.path.startsWith('/css/') ||
    req.path.startsWith('/js/') ||
    req.path.startsWith('/fonts/')
  ) {
    next();
    return;
  }

  // Allow the auth check endpoint
  if (req.path === '/api/auth/check') {
    next();
    return;
  }

  const token = extractToken(req);
  if (token && sessions.has(token)) {
    next();
    return;
  }

  res.status(401).json({ error: 'Authentication required' });
}

/**
 * Validate WebSocket upgrade request.
 * Checks session token in cookie or query string.
 */
export function validateWSAuth(req: { headers: Record<string, string | string[] | undefined>; url?: string }): boolean {
  // Check cookie
  const cookieHeader = (req.headers.cookie as string) || '';
  const match = cookieHeader.match(/(?:^|;\s*)session=([a-f0-9]+)/);
  if (match && sessions.has(match[1])) return true;

  // Check query string ?token=xxx
  if (req.url) {
    try {
      const url = new URL(req.url, 'http://localhost');
      const token = url.searchParams.get('token');
      if (token && sessions.has(token)) return true;
    } catch { /* ignore */ }
  }

  return false;
}

/**
 * Ensure WEB_USER and WEB_PASS are configured.
 * Call at startup — exits if credentials are missing.
 */
export function requireCredentials(): void {
  const user = process.env.WEB_USER;
  const pass = process.env.WEB_PASS;
  if (!user || !pass) {
    console.error('\n  ✖ WEB_USER and WEB_PASS must be set in .env or environment.\n');
    console.error('  The web panel requires authentication. Set both variables and restart.\n');
    process.exit(1);
  }
}
