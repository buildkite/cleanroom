#!/bin/sh

set -eu

port="${1:-8143}"
cert_path="${CLEANROOM_EXPOSURE_CERT:-${XDG_CONFIG_HOME:-$HOME/.config}/cleanroom/tls/exposure-cert.pem}"
if [ ! -f "$cert_path" ]; then
  echo "exposure certificate not found: $cert_path" >&2
  echo "run the example once to generate it, or set CLEANROOM_EXPOSURE_CERT" >&2
  exit 1
fi

curl_verify() {
  curl --silent --show-error --cacert "$cert_path" "$@"
}

echo "== exact route =="
curl_verify "https://example.cleanroom.localhost:${port}/"
echo

echo "== wildcard app route =="
curl_verify "https://app.example.cleanroom.localhost:${port}/"
echo

echo "== wildcard redirect route =="
curl_verify --dump-header - --output /dev/null "https://s3.example.cleanroom.localhost:${port}/"
echo
