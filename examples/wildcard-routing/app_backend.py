import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        payload = {
            "host": self.headers.get("Host", ""),
            "path": self.path,
            "x_forwarded_for": self.headers.get("X-Forwarded-For", ""),
            "x_forwarded_host": self.headers.get("X-Forwarded-Host", ""),
            "x_forwarded_port": self.headers.get("X-Forwarded-Port", ""),
            "x_forwarded_proto": self.headers.get("X-Forwarded-Proto", ""),
        }
        body = json.dumps(payload, indent=2, sort_keys=True) + "\n"
        encoded = body.encode("utf-8")

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, fmt, *args):
        return


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 18080), Handler).serve_forever()
