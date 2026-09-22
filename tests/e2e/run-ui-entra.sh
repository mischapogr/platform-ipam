#!/usr/bin/env sh
# `entra` mode end-to-end runner (package A4, ADR 0006,
# docs/GUI_AUTHENTICATION.md "Entra ID mode (package A4)").
#
# Mirrors tests/e2e/run-ui-ldap.sh (package A5): this never touches the
# shared development project (platform-ipam-dev) other packages are using
# concurrently. It brings up deploy/compose/compose.ui-entra.yaml in its OWN
# throw-away Compose project, starting only the NetBox-side services the
# overlay needs (never the platform api/worker, which already publish the
# host's port 8080 in the main project), and tears that project down --
# containers and volumes -- when it finishes, pass or fail.
#
# It never assumes a host loopback port is reachable from wherever it runs:
# the test itself (tests/e2e/ui_mode_entra.py) executes inside a throw-away
# container attached to the project's own Docker network, talking to
# `ui-proxy`, `mock-oidc` and `netbox` by their Compose service names -- the
# same way the platform adapter does, and the same way a real oauth2-proxy
# deployment would resolve its issuer.
#
# PHASED INTERFACE. A from-scratch NetBox first-run migration can take well
# past a caller's own timeout (10-15 minutes is not unusual under load), so
# this script never blocks inside a single call waiting for it. Run one
# subcommand at a time:
#
#   ./run-ui-entra.sh init       # (re)writes $TMPDIR/a4.env from .env -- fast
#   ./run-ui-entra.sh up         # starts netbox-db/redis/redis-cache/netbox
#                                 # only -- returns as soon as they exist, it
#                                 # does NOT wait for netbox to become healthy
#   ./run-ui-entra.sh wait-netbox  # polls (up to ~9 minutes) and prints the
#                                 # migration log tail; exit 0 once healthy,
#                                 # exit 1 (not yet -- re-run) otherwise.
#                                 # Repeat until it prints HEALTHY.
#   ./run-ui-entra.sh bootstrap  # netbox-bootstrap; then starts ui-proxy,
#                                 # oauth2-proxy, mock-oidc (fast: netbox is
#                                 # already healthy by now, so ui-proxy's own
#                                 # dependency wait is instant)
#   ./run-ui-entra.sh wait-issuer  # polls (up to ~2 minutes) for mock-oidc
#                                 # and oauth2-proxy to accept connections
#   ./run-ui-entra.sh test       # runs tests/e2e/ui_mode_entra.py
#   ./run-ui-entra.sh down       # tears the project down (containers +
#                                 # volumes) and shows it is gone
#
# Or, with no argument (or `all`), every phase above runs in one call, each
# with its own generous internal deadline -- the one-shot mode for a human at
# a terminal, who has no external per-call time limit.
#
# Why splitting `up` from waiting matters: `docker compose up -d` blocks
# until every *targeted* service with a `depends_on: X: condition:
# service_healthy` on another *targeted* service sees that dependency turn
# healthy -- and aborts the whole call the moment it observes `unhealthy`,
# rather than continuing to poll. ui-proxy depends on netbox that way
# (compose.netbox.yaml). netbox's own healthcheck budget (interval 15s,
# retries 12, start_period 90s -- about 4.5 minutes) is comfortably enough
# once its image is cached, but not for a cold pull plus first-run migrations
# under concurrent load, so bringing ui-proxy up in the same call as netbox
# can make `up` itself fail well before migrations are anywhere near done.
# `up` (phase_up) therefore targets only netbox-db/redis/redis-cache/netbox;
# ui-proxy is not started until `bootstrap` (phase_bootstrap), by which point
# wait-netbox has already confirmed netbox healthy on this script's own,
# longer, no-abort-on-transient-unhealthy polling loop.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE_DIR="$ROOT/deploy/compose"
cd "$COMPOSE_DIR"

case "${IPAM_NETBOX_CANDIDATE:-0}" in
  0) PROJECT=platform-ipam-a4; UI_PORT=18084 ;;
  1) PROJECT=platform-ipam-a4-4610; UI_PORT=18104 ;;
  *) echo "IPAM_NETBOX_CANDIDATE must be 0 or 1" >&2; exit 2 ;;
esac
WORK_ENV="${TMPDIR:-/tmp}/${PROJECT}.env"

compose() {
  if [ "${IPAM_NETBOX_CANDIDATE:-0}" = 1 ]; then
    docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
      -f compose.yaml -f compose.netbox.yaml -f compose.ui-entra.yaml \
      -f compose.netbox-compat-4_6_10.yaml "$@"
  else
    docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
      -f compose.yaml -f compose.netbox.yaml -f compose.ui-entra.yaml "$@"
  fi
}

phase_init() {
  [ -f .env ] || { echo "deploy/compose/.env is missing; run ./create-env.sh" >&2; exit 2; }
  # A dedicated copy of .env with only the two values this isolation scheme
  # needs changed: the project name (so `down -v` can never reach the shared
  # platform-ipam-dev project's containers/volumes) and the published port
  # (platform-ipam-dev's ui-proxy already holds 18000/NETBOX_PORT's default;
  # platform-ipam-a5's run-ui-ldap.sh already holds 18085). Always
  # overwritten from the current .env, so a variable added there since the
  # last run is picked up.
  cp .env "$WORK_ENV"
  if grep -q '^COMPOSE_PROJECT_NAME=' "$WORK_ENV"; then
    sed -i.bak "s/^COMPOSE_PROJECT_NAME=.*/COMPOSE_PROJECT_NAME=${PROJECT}/" "$WORK_ENV" && rm -f "$WORK_ENV.bak"
  else
    echo "COMPOSE_PROJECT_NAME=${PROJECT}" >>"$WORK_ENV"
  fi
  if grep -q '^NETBOX_PORT=' "$WORK_ENV"; then
    sed -i.bak "s/^NETBOX_PORT=.*/NETBOX_PORT=${UI_PORT}/" "$WORK_ENV" && rm -f "$WORK_ENV.bak"
  else
    echo "NETBOX_PORT=${UI_PORT}" >>"$WORK_ENV"
  fi

  # shellcheck disable=SC1090
  set -a; . "$WORK_ENV"; set +a
  : "${IPAM_ENVIRONMENT:?IPAM_ENVIRONMENT must be set}"
  if [ "$IPAM_ENVIRONMENT" != "development" ]; then
    echo "refusing to run against a non-development environment" >&2
    exit 2
  fi

  echo "==> validating the overlay"
  compose config -q
  echo "==> wrote ${WORK_ENV}"
}

phase_up() {
  [ -f "$WORK_ENV" ] || { echo "${WORK_ENV} is missing; run './run-ui-entra.sh init' first" >&2; exit 2; }
  echo "==> starting netbox-db, netbox-redis, netbox-redis-cache, netbox"
  echo "    (deliberately NOT ui-proxy/oauth2-proxy/mock-oidc here -- see the"
  echo "    header comment for why)"
  compose up -d netbox-db netbox-redis netbox-redis-cache netbox
}

phase_wait_netbox() {
  echo "==> polling for NetBox health (up to ~9 minutes this call; first-run"
  echo "    migrations commonly take 5-15 minutes in total -- re-run this"
  echo "    phase if it exits 1)"
  deadline=$(( $(date +%s) + 540 ))
  health=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    health=$(compose ps --format '{{.Health}}' netbox 2>/dev/null || echo "")
    if [ "$health" = "healthy" ]; then
      echo "HEALTHY"
      return 0
    fi
    sleep 5
  done
  echo "not healthy yet (last status: '${health:-unknown}'); log tail:"
  compose logs --tail=5 netbox || true
  return 1
}

phase_bootstrap() {
  echo "==> bootstrapping NetBox credentials and the operator groups"
  compose --profile bootstrap run --rm netbox-bootstrap
  echo "==> starting ui-proxy, oauth2-proxy, mock-oidc"
  # netbox is already healthy (wait-netbox above), so ui-proxy's own
  # depends_on wait here is immediate, not a repeat of the earlier risk.
  compose up -d ui-proxy oauth2-proxy mock-oidc
}

phase_wait_issuer() {
  echo "==> resolving the project's Docker network"
  NETWORK=$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" --format '{{.Name}}' | head -n1)
  [ -n "$NETWORK" ] || { echo "could not find a Docker network for project ${PROJECT}" >&2; return 1; }
  echo "network: ${NETWORK}"

  echo "==> polling for the mock OIDC issuer and oauth2-proxy (up to ~2 minutes)"
  # oauth2-proxy fetches the issuer's discovery document at startup and can
  # fail its first attempt if mock-oidc was not yet accepting connections;
  # compose.ui-entra.yaml gives it `restart: on-failure` to converge.
  deadline=$(( $(date +%s) + 120 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if docker run --rm --network "$NETWORK" docker.io/python:3.12-alpine \
        python3 -c "
import urllib.request
urllib.request.urlopen('http://mock-oidc:8080/default/.well-known/openid-configuration', timeout=5)
urllib.request.urlopen('http://oauth2-proxy:4180/ping', timeout=5)
" >/dev/null 2>&1; then
      echo "READY"
      return 0
    fi
    sleep 5
  done
  echo "mock-oidc/oauth2-proxy did not become ready" >&2
  return 1
}

phase_test() {
  NETWORK=$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" --format '{{.Name}}' | head -n1)
  [ -n "$NETWORK" ] || { echo "could not find a Docker network for project ${PROJECT}" >&2; return 1; }
  # shellcheck disable=SC1090
  set -a; . "$WORK_ENV"; set +a
  echo "==> running the entra-mode security checks"
  docker run --rm --network "$NETWORK" \
    -e IPAM_E2E_UI_ENTRA=1 \
    -e NETBOX_UI_ENTRA_BASE_URL=http://ui-proxy:8080 \
    -e NETBOX_INTERNAL_URL=http://netbox:8080 \
    -e IPAM_NETBOX_TOKEN="$IPAM_NETBOX_TOKEN" \
    -v "$ROOT/tests/e2e/ui_mode_entra.py:/ui_mode_entra.py:ro" \
    docker.io/python:3.12-alpine \
    python3 -m unittest /ui_mode_entra.py -v
}

phase_down() {
  echo "==> tearing down project ${PROJECT}"
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  echo "==> containers left in ${PROJECT} (should be empty):"
  docker compose -p "$PROJECT" ps -a 2>/dev/null || true
  echo "==> volumes left for ${PROJECT} (should be empty):"
  docker volume ls | grep "$PROJECT" || echo "(none)"
  rm -f "$WORK_ENV"
}

phase_stop() {
  compose stop
}

case "${1:-all}" in
  init) phase_init ;;
  up) phase_up ;;
  wait-netbox) phase_wait_netbox ;;
  bootstrap) phase_bootstrap ;;
  wait-issuer) phase_wait_issuer ;;
  test) phase_test ;;
  stop) phase_stop ;;
  down) phase_down ;;
  all)
    # One-shot mode: every phase in order, tearing down on any failure too.
    cleanup() {
      status=$?
      phase_down
      exit "$status"
    }
    trap cleanup EXIT INT TERM
    phase_init
    phase_up
    tries=0
    until phase_wait_netbox; do
      tries=$((tries + 1))
      [ "$tries" -lt 10 ] || { echo "NetBox did not become healthy" >&2; exit 1; }
    done
    phase_bootstrap
    phase_wait_issuer
    phase_test
    ;;
  *)
    echo "usage: $0 [init|up|wait-netbox|bootstrap|wait-issuer|test|stop|down|all]" >&2
    exit 2
    ;;
esac
