from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import quote


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

    def log_message(self, fmt, *args):
        return


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 18081), Handler).serve_forever()
