#!/usr/bin/env python3
"""Local-only deterministic HTTP fixture for test-telemost-multi-client.sh."""
import http.server
import sys
import time
import urllib.parse


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urllib.parse.urlsplit(self.path)
        parts = parsed.path.strip('/').split('/')
        if len(parts) != 2 or parts[0] not in ('client', 'stream') or not parts[1].isdigit():
            self.send_error(404)
            return
        line = ('telemost-client-' + parts[1] + '\n').encode('ascii')
        seconds = 1
        if parts[0] == 'stream':
            try:
                seconds = int(urllib.parse.parse_qs(parsed.query).get('seconds', ['90'])[0])
            except ValueError:
                self.send_error(400)
                return
            if not 1 <= seconds <= 300:
                self.send_error(400)
                return
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.send_header('Content-Length', str(len(line) * seconds))
        self.end_headers()
        try:
            for _ in range(seconds):
                self.wfile.write(line)
                self.wfile.flush()
                if parts[0] == 'stream':
                    time.sleep(1)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass


if __name__ == '__main__':
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    print(server.server_port, flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
