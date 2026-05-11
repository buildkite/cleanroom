#!/bin/sh

set -eu

port="${1:-8143}"
cert_path="${CLEANROOM_EXPOSURE_CERT:-${XDG_CONFIG_HOME:-$HOME/.config}/cleanroom/tls/exposure-cert.pem}"

curl_verify() {
  if [ -f "$cert_path" ]; then
    curl --silent --show-error --fail-with-body --cacert "$cert_path" "$@"
    return
  fi
  curl --silent --show-error --fail-with-body "$@"
}

echo "== exact route =="
curl_verify "https://example.cleanroom.localhost:${port}/"
echo

echo "== app route =="
curl_verify "https://example-app.cleanroom.localhost:${port}/"
echo

echo "== redirect route =="
curl_verify --dump-header - --output /dev/null "https://example-s3.cleanroom.localhost:${port}/"
echo
