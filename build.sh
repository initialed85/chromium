#!/usr/bin/env bash
# Build + push the Chromium image on the x86 Docker daemon so the cluster gets
# an amd64 image (this machine is arm64).
set -euo pipefail

DOCKER_HOST="${DOCKER_HOST:-tcp://192.168.11.128:2375}"
export DOCKER_HOST
TAG="${TAG:-latest}"

cd "$(dirname "$0")"

echo "==> building initialed85/chromium:${TAG}"
docker build --platform=linux/amd64 -t "initialed85/chromium:${TAG}" .
docker push "initialed85/chromium:${TAG}"
