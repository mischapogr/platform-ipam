#!/usr/bin/env sh
# Optional AWS wire-protocol test. Moto has its own isolated Compose project;
# the development worker's deterministic fake remains the default.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$ROOT/deploy/compose"
[ -f .env ] || { echo "deploy/compose/.env is missing" >&2; exit 2; }
PROJECT=platform-ipam-ls2
compose() {
  docker compose --env-file .env -p "$PROJECT" -f compose.yaml -f compose.aws-moto.yaml "$@"
}
case "${1:-}" in
  up)
    compose config -q
    compose up -d moto
    ;;
  test)
    [ "$(compose ps --format '{{.Health}}' moto)" = healthy ] || { echo "Moto is not healthy" >&2; exit 3; }
    docker run --rm --network "${PROJECT}_default" \
      -e IPAM_TEST_MOTO_URL=http://moto:5000 \
      -v "$ROOT:/workspace:ro" -w /workspace \
      docker.io/golang:1.26.8-bookworm \
      go test ./internal/cloud -run TestMotoEC2STSProtocol -count=1 -v
    ;;
  stop)
    compose stop
    ;;
  *)
    echo "usage: $0 {up|test|stop}" >&2
    exit 2
    ;;
esac
