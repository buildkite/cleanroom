#!/bin/sh

set -eu

apt-get update
apt-get install --yes --no-install-recommends nginx python3

python3 ./app_backend.py &
app_pid=$!

python3 ./redirect_backend.py &
redirect_pid=$!

cleanup() {
  kill "$app_pid" "$redirect_pid" 2>/dev/null || true
}

trap cleanup EXIT INT TERM

nginx -c "$PWD/nginx.conf" -g 'daemon off;'
