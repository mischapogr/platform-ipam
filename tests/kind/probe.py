"""Chart controller probe only; Compose tests run the actual application."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import os
import sys
import time


mode = sys.argv[1]
if mode == "migrate":
    if "fail-migration" in os.environ.get("IPAM_DATABASE_URL", ""):
        print("local migration probe failed as requested", flush=True)
        sys.exit(7)
    print("local migration probe completed", flush=True)
    time.sleep(2)
    sys.exit(0)
if mode == "seed":
    print("local seed probe completed", flush=True)
    sys.exit(0)
if mode not in ("api", "worker"):
    sys.exit(2)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path in ("/livez", "/readyz"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(mode.encode())
        else:
            self.send_error(404)


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
