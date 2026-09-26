#!/usr/bin/env sh
# Isolated NetBox patch-version probe; keeps volumes across stop/restart.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
PROJECT=platform-ipam-ls4
export IPAM_API_PORT=18090 NETBOX_PORT=18091 COMPOSE_PROJECT_NAME="$PROJECT"
cd "$ROOT/deploy/compose"
[ -f .env ] || { echo "deploy/compose/.env is missing" >&2; exit 2; }
compose() {
  docker compose --env-file .env -p "$PROJECT" -f compose.yaml \
    -f compose.netbox.yaml -f compose.netbox-compat-4_6_10.yaml "$@"
}
case "${1:-}" in
  up)
    compose config -q
    # A fresh NetBox migration can exceed the default dependency health window.
    compose up -d netbox
    ;;
  wait)
    [ "$(compose ps --format '{{.Health}}' netbox)" = healthy ] || {
      echo "NetBox is still migrating or unhealthy; inspect the short service log" >&2
      exit 3
    }
    compose up -d
    ;;
  bootstrap)
    compose --profile bootstrap run --rm netbox-bootstrap
    compose --profile bootstrap run --rm seed
    ;;
  test)
    for service in netbox api ui-proxy; do
      [ "$(compose ps --format '{{.Health}}' "$service")" = healthy ] || {
        echo "$service is not healthy; refusing a skipped compatibility run" >&2
        exit 3
      }
    done
    cd "$ROOT"
    export IPAM_E2E_RUN_ID="${IPAM_E2E_RUN_ID:-ls4$(date -u +%Y%m%d%H%M%S)}"
    for module in test_e2e_netbox_roles.py test_e2e_allocation.py test_e2e_ui_proxy.py; do
      python3 -m unittest discover -s tests/e2e -p "$module" -v
    done
    ;;
  stop)
    compose stop
    ;;
  *)
    echo "usage: $0 {up|wait|bootstrap|test|stop}" >&2
    exit 2
    ;;
esac
