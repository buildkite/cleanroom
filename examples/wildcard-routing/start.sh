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
nginx -c "$PWD/nginx.conf" -g 'daemon off;'
