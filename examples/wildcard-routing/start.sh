#!/bin/sh

set -eu

log() {
  printf '%s wildcard-start: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

dump_listeners() {
  log "listener snapshot"
  if command -v ss >/dev/null 2>&1; then
    ss -ltnp >&2 || true
  elif command -v netstat >/dev/null 2>&1; then
    netstat -ltnp >&2 || true
  else
    cat /proc/net/tcp >&2 || true
  fi
}

dump_processes() {
  log "process snapshot"
  ps -ef >&2 || true
}

probe_guest_http() {
  log "guest-local http probe"
  python3 - <<'PY' >&2 || true
import http.client

for host in (
    "example.cleanroom.localhost",
    "app.example.cleanroom.localhost",
    "s3.example.cleanroom.localhost",
):
    print(f"guest-local probe {host}: starting", flush=True)
    try:
        conn = http.client.HTTPConnection("127.0.0.1", 80, timeout=3)
        conn.request("GET", "/", headers={"Host": host})
        resp = conn.getresponse()
        body = resp.read(512).decode("utf-8", "replace")
        headers = ", ".join(f"{k}: {v}" for k, v in resp.getheaders())
        print(f"guest-local probe {host}: status={resp.status} reason={resp.reason}", flush=True)
        print(f"guest-local probe {host}: headers={headers}", flush=True)
        print(f"guest-local probe {host}: body={body!r}", flush=True)
    except Exception as err:
        print(f"guest-local probe {host}: error={err!r}", flush=True)
    finally:
        try:
            conn.close()
        except Exception:
            pass
PY
}

post_nginx_start_probe() {
  sleep 2
  log "post-nginx startup probe"
  dump_processes
  dump_listeners
  probe_guest_http
  sleep 5
  log "post-nginx follow-up probe"
  dump_processes
  dump_listeners
  probe_guest_http
}

log "starting package installation"
apt-get update
apt-get install --yes --no-install-recommends nginx python3
log "package installation complete"
log "nginx version: $(nginx -v 2>&1)"
log "testing nginx config"
nginx -t -c "$PWD/nginx.conf" >&2
log "dumping nginx config"
nginx -T -c "$PWD/nginx.conf" >&2
dump_listeners

log "starting app backend"
python3 -u ./app_backend.py &
app_pid=$!
log "app backend pid=$app_pid"
sleep 1
if ! kill -0 "$app_pid" 2>/dev/null; then
  log "app backend exited immediately"
  wait "$app_pid"
fi
dump_listeners

log "starting redirect backend"
python3 -u ./redirect_backend.py &
redirect_pid=$!
log "redirect backend pid=$redirect_pid"
sleep 1
if ! kill -0 "$redirect_pid" 2>/dev/null; then
  log "redirect backend exited immediately"
  wait "$redirect_pid"
fi
dump_listeners

cleanup() {
  log "cleaning up backend processes"
  kill "$app_pid" "$redirect_pid" 2>/dev/null || true
}

trap cleanup EXIT INT TERM

log "starting nginx foreground server"
dump_listeners
post_nginx_start_probe &
nginx -c "$PWD/nginx.conf" -g 'daemon off;'
