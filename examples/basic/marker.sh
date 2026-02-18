#!/bin/sh
set -eu

date -u +"marker created at %Y-%m-%dT%H:%M:%SZ" > .marker-created
echo "created .marker-created"
