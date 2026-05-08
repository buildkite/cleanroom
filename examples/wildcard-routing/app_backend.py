import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import quote


def host_without_port(host):
    return host.rsplit(":", 1)[0]


def forwarded_location(headers):
    client_chain = headers.get("X-Forwarded-For", "").strip()
    if not client_chain:
        return "", "missing X-Forwarded-For"

    proto = headers.get("X-Forwarded-Proto", "http").strip() or "http"
    port = headers.get("X-Forwarded-Port", "").strip()

    authority = "app.example.cleanroom.localhost"
    if port:
        authority = f"{authority}:{port}"

    return (
        f"{proto}://{authority}/from-s3?client={quote(client_chain, safe='')}",
        "",
    )


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        host = self.headers.get("Host", "")
        route_host = host_without_port(host)

        if route_host == "example.cleanroom.localhost":
            body = b"exact route ok\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        if route_host == "s3.example.cleanroom.localhost":
            location, error = forwarded_location(self.headers)
            if error:
                encoded = (error + "\n").encode("utf-8")
                self.send_response(400)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)
                return

            body = f"redirecting to {location}\n".encode("utf-8")
            self.send_response(302)
            self.send_header("Location", location)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        if route_host != "app.example.cleanroom.localhost":
            self.send_response(404)
            self.end_headers()
            return

        payload = {
            "host": host,
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
    HTTPServer(("0.0.0.0", 80), Handler).serve_forever()
