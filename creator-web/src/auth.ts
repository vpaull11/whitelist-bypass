import type { Request, Response, NextFunction } from 'express';

/**
 * HTTP Basic Auth middleware.
 * Reads credentials from WEB_USER / WEB_PASS env variables.
 * If neither is set, authentication is disabled (open access).
 */
export function authMiddleware(req: Request, res: Response, next: NextFunction): void {
  const user = process.env.WEB_USER;
  const pass = process.env.WEB_PASS;

  // Skip auth when no credentials configured
  if (!user && !pass) {
    next();
    return;
  }

  const header = req.headers.authorization;
  if (!header || !header.startsWith('Basic ')) {
    res.set('WWW-Authenticate', 'Basic realm="Creator Web Panel"');
    res.status(401).json({ error: 'Authentication required' });
    return;
  }

  const decoded = Buffer.from(header.slice(6), 'base64').toString('utf8');
  const [u, ...pParts] = decoded.split(':');
  const p = pParts.join(':'); // password may contain colons

  if (u === user && p === pass) {
    next();
  } else {
    res.set('WWW-Authenticate', 'Basic realm="Creator Web Panel"');
    res.status(401).json({ error: 'Invalid credentials' });
  }
}

/**
 * Validate WebSocket upgrade request with Basic Auth.
 * Returns true if the connection is allowed.
 */
export function validateWSAuth(req: { headers: Record<string, string | string[] | undefined> }): boolean {
  const user = process.env.WEB_USER;
  const pass = process.env.WEB_PASS;
  if (!user && !pass) return true;

  const header = req.headers.authorization;
  if (!header || typeof header !== 'string' || !header.startsWith('Basic ')) return false;

  const decoded = Buffer.from(header.slice(6), 'base64').toString('utf8');
  const [u, ...pParts] = decoded.split(':');
  const p = pParts.join(':');
  return u === user && p === pass;
}
