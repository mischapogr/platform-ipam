#!/usr/bin/env sh
# `onboard apply --aws-objects` end-to-end runner (package N3,
# docs/WORK_PLAN.md Track N; ADR 0009).
#
# Unlike run-e2e.sh, this script never touches the shared development
# project (platform-ipam-dev): it brings up the FULL platform stack --
# compose.yaml + compose.netbox.yaml + compose.netbox-plugin.yaml (the
# optional overlay that builds NetBox with netbox-aws-vpc-plugin installed,
# package N2) -- in its OWN throw-away Compose project, and tears that
# project down (containers and volumes) when it finishes, pass or fail. It
# never assumes a host loopback port is reachable from wherever it runs: the
# `onboard` CLI invocations below run *inside* the already-running `api`
# container (docker compose exec, which has both the platform-ipam binary
# and its IPAM_* environment already set), and the verification suite
# (tests/e2e/plugin_mode_import.py) runs inside a throw-away container
# attached to the project's own Docker network, talking to `api` and
# `netbox` by their Compose service names -- the same way the platform
# adapter itself does, and the same pattern tests/e2e/run-ui-ldap.sh uses.
#
# plugin_mode_import.py is named the way it is -- not test_e2e_plugin_import.py
# -- specifically so run-e2e.sh's `test_e2e_*.py` glob never sweeps it into
# a run against the shared platform-ipam-dev stack, which never builds the
# plugin overlay; it is invoked here directly, by path.
#
# PHASED INVOCATION. A from-scratch NetBox install runs its migrations
# before it starts answering its healthcheck at all, and that can take much
# longer than any one shell/CI step is willing to block for (especially
# under load from other concurrent NetBox instances). A single `docker
# compose up -d` for the WHOLE stack is also the wrong tool for waiting it
# out: compose.netbox.yaml's `ui-proxy`/`api`/`worker` all declare
# `depends_on: netbox: condition: service_healthy`, and Compose treats a
# dependency that reaches Docker's "unhealthy" status (not just "still
# starting") as a hard failure for `up` -- it does not keep waiting, it
# aborts with "dependency failed to start". NetBox's own healthcheck retry
# budget (deploy/compose/compose.netbox.yaml) is far shorter than a slow
# migration, so a naive single `up -d --build` call is not reliable here.
#
# This script is therefore split into phases, each independently
# re-invocable and individually well under any one command's own timeout:
#   build          build every image (no health waiting at all)
#   netbox-up      start ONLY netbox's own dependency chain (netbox-db,
#                  netbox-redis, netbox-redis-cache, netbox) -- nothing that
#                  depends on netbox's health, so this returns immediately
#                  once netbox itself is *starting*, never blocking on it
#   netbox-wait    poll netbox's health for up to ~8 minutes and return,
#                  printing the migration log tail; prints NETBOX_HEALTHY=1
#                  or NETBOX_HEALTHY=0 on its last line -- re-invoke this
#                  phase (not the whole script) until it reports healthy
#   rest-up        bring up everything else (now that netbox is healthy,
#                  every depends_on: service_healthy check passes at once)
#   bootstrap      netbox-bootstrap + seed (credentials, groups, pools)
#   import         onboard parse/plan/apply (twice) through the api
#                  container, with inline assertions
#   verify         run tests/e2e/plugin_mode_import.py in a throw-away
#                  container on the project's network
#   down           tear the project down and confirm it is gone
#   all            the full sequence above, one shot, for a human running
#                  this interactively with time to spare (loops netbox-wait
#                  itself; still refuses to hang forever)
#
# `./run-plugin-import.sh` with no argument means `all`.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE_DIR="$ROOT/deploy/compose"
cd "$COMPOSE_DIR"

[ -f .env ] || { echo "deploy/compose/.env is missing; run ./create-env.sh" >&2; exit 2; }

: "${TMPDIR:=/tmp}"
PROJECT=platform-ipam-plugin-import
# Fixed (not mktemp) paths: phases are separate, independent invocations of
# this script (no shared shell state between them, only the filesystem and
# whatever Docker itself tracks for the project), so every phase must be
# able to reconstruct the same env/override deterministically.
WORK_ENV="${TMPDIR}/n3-plugin-import.env"
OVERRIDE="${TMPDIR}/n3-plugin-import-override.yaml"
TABLE_PATH=/tmp/n3-table.json

write_work_files() {
  # A dedicated copy of .env with only the values this isolation scheme
  # needs changed: the project name (so `down -v` can never reach the
  # shared platform-ipam-dev project's containers/volumes) and NetBox's
  # published port (platform-ipam-dev's ui-proxy already holds
  # NETBOX_PORT's default). Regenerated at the start of every phase so no
  # phase depends on a previous one having run in the same shell.
  cp .env "$WORK_ENV"
  if grep -q '^COMPOSE_PROJECT_NAME=' "$WORK_ENV"; then
    sed -i.bak "s/^COMPOSE_PROJECT_NAME=.*/COMPOSE_PROJECT_NAME=${PROJECT}/" "$WORK_ENV" && rm -f "$WORK_ENV.bak"
  else
    echo "COMPOSE_PROJECT_NAME=${PROJECT}" >>"$WORK_ENV"
  fi
  if grep -q '^NETBOX_PORT=' "$WORK_ENV"; then
    sed -i.bak 's/^NETBOX_PORT=.*/NETBOX_PORT=18087/' "$WORK_ENV" && rm -f "$WORK_ENV.bak"
  else
    echo "NETBOX_PORT=18087" >>"$WORK_ENV"
  fi

  # compose.yaml hard-codes the `api` service's published port as
  # 127.0.0.1:8080:8080 (not parameterized like NETBOX_PORT), which the
  # shared platform-ipam-dev project already holds on most developer
  # machines. This throw-away project never needs `api` reachable from the
  # host -- every request below goes over the project's own Docker network
  # -- so the override removes the publish entirely rather than risk a bind
  # conflict. Compose's `!override` YAML tag (Compose Specification's
  # merge-override extension) replaces the base file's `ports` list instead
  # of merging into it.
  cat >"$OVERRIDE" <<'YAML'
services:
  api:
    ports: !override []
YAML
}

write_work_files
# shellcheck disable=SC1090
set -a; . "$WORK_ENV"; set +a
: "${IPAM_ENVIRONMENT:?IPAM_ENVIRONMENT must be set}"
if [ "$IPAM_ENVIRONMENT" != "development" ]; then
  echo "refusing to run against a non-development environment" >&2
  exit 2
fi

compose() {
  docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
    -f compose.yaml -f compose.netbox.yaml -f compose.netbox-plugin.yaml -f "$OVERRIDE" "$@"
}

phase_build() {
  echo "==> validating the overlay"
  compose config -q
  echo "==> building every image"
  compose build
}

phase_netbox_up() {
  echo "==> starting only netbox's own dependency chain (nothing that depends on netbox's health)"
  compose up -d netbox-db netbox-redis netbox-redis-cache netbox
}

phase_netbox_wait() {
  echo "==> polling NetBox health for up to ~8 minutes (first run needs several minutes for migrations)"
  deadline=$(( $(date +%s) + 480 ))
  healthy=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    status=$(compose ps --format '{{.Health}}' netbox 2>/dev/null || true)
    echo "  netbox health: ${status:-starting}"
    if [ "$status" = "healthy" ]; then
      healthy=1
      break
    fi
    if [ "$status" = "unhealthy" ]; then
      echo "==> netbox is 'unhealthy' -- last 40 log lines:"
      compose logs --tail 40 netbox || true
      # Docker healthcheck 'unhealthy' is not terminal: the check keeps
      # running and can still recover once migrations finish, so this phase
      # keeps polling instead of giving up immediately -- but the log tail
      # above is printed every time so a genuinely broken start is visible.
    fi
    sleep 10
  done
  if [ "$healthy" = "1" ]; then
    echo "==> NetBox is healthy"
    echo "NETBOX_HEALTHY=1"
  else
    echo "==> NetBox is not healthy yet -- last 60 log lines:"
    compose logs --tail 60 netbox || true
    echo "NETBOX_HEALTHY=0"
  fi
}

phase_rest_up() {
  echo "==> netbox is healthy; bringing up the rest of the stack (api, worker, netbox-worker, ui-proxy)"
  compose up --build -d
}

phase_bootstrap() {
  echo "==> bootstrapping NetBox credentials and seeding pools/custom fields"
  compose --profile bootstrap run --rm netbox-bootstrap
  compose --profile bootstrap run --rm seed
  echo "==> confirming the plugin API answers before importing anything"
  compose exec -T api curl -fsS -m 10 -H "Authorization: Token ${IPAM_NETBOX_TOKEN}" \
    http://netbox:8080/api/plugins/aws-vpc/aws-accounts/ >/dev/null
  echo "==> waiting for the platform API"
  deadline=$(( $(date +%s) + 180 ))
  until compose exec -T api curl -fsS -m 5 http://127.0.0.1:8080/readyz >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "the API did not become ready" >&2; exit 1; }
    sleep 2
  done
  echo "==> platform API is ready"
}

assert_count() {
  desc=$1; pattern=$2; want=$3; text=$4
  got=$(printf '%s\n' "$text" | grep -c -E -- "$pattern" || true)
  if [ "$got" != "$want" ]; then
    echo "FAIL: $desc: expected $want, got $got" >&2
    printf '%s\n' "$text" >&2
    exit 1
  fi
  echo "OK: $desc ($got)"
}

phase_import() {
  # One VPC CIDR imported under two AWS accounts (onboard.Plan's
  # duplicate-CIDR rule collapses it to one NetBox prefix -- ADR 0009's
  # central scenario) plus one subnet of it, linked by parent_id. Must
  # match the constants at the top of tests/e2e/plugin_mode_import.py.
  # 10.64.0.0/22 is deliberately pool_dev_euc1's own first /22 block
  # (deploy/compose/fixtures/pools.yaml is 10.64.0.0/16), so a reservation
  # afterwards would receive it first if occupancy did not block it.
  NETWORKS_CSV='cidr,account_id,region,type,resource_id,parent_id,name
10.64.0.0/22,111111111111,eu-central-1,vpc,vpc-acct1,,shared-vpc-account1
10.64.0.0/22,222222222222,eu-central-1,vpc,vpc-acct2,,shared-vpc-account2
10.64.0.0/24,111111111111,eu-central-1,subnet,subnet-acct1,vpc-acct1,shared-subnet'

  echo "==> onboard parse (stdin -> table.json inside the api container)"
  printf '%s\n' "$NETWORKS_CSV" | compose exec -T api /usr/local/bin/platform-ipam onboard parse - --out "$TABLE_PATH"

  echo "==> onboard plan (must report zero errors)"
  compose exec -T api /usr/local/bin/platform-ipam onboard plan "$TABLE_PATH" --domain local-development >/dev/null

  echo "==> onboard apply --aws-objects, first run (everything created)"
  APPLY1=$(compose exec -T api /usr/local/bin/platform-ipam onboard apply "$TABLE_PATH" \
    --domain local-development --batch e2e-plugin-import --aws-objects)
  printf '%s\n' "$APPLY1"
  assert_count "first apply: prefixes created"     '"kind":"prefix".*"action":"created"'      2 "$APPLY1"
  assert_count "first apply: aws-accounts created" '"kind":"aws-account".*"action":"created"' 2 "$APPLY1"
  assert_count "first apply: aws-region created"   '"kind":"aws-region".*"action":"created"'  1 "$APPLY1"
  assert_count "first apply: aws-vpcs created"     '"kind":"aws-vpc".*"action":"created"'     2 "$APPLY1"
  assert_count "first apply: aws-subnet created"   '"kind":"aws-subnet".*"action":"created"'  1 "$APPLY1"

  echo "==> onboard apply --aws-objects, second run (must be all unchanged, no prefix writes)"
  APPLY2=$(compose exec -T api /usr/local/bin/platform-ipam onboard apply "$TABLE_PATH" \
    --domain local-development --batch e2e-plugin-import --aws-objects)
  printf '%s\n' "$APPLY2"
  assert_count "second apply: no prefix lines (already-unmanaged, dropped from the write set)" '"kind":"prefix"' 0 "$APPLY2"
  assert_count "second apply: aws-accounts unchanged" '"kind":"aws-account".*"action":"unchanged"' 2 "$APPLY2"
  assert_count "second apply: aws-region unchanged"   '"kind":"aws-region".*"action":"unchanged"'  1 "$APPLY2"
  assert_count "second apply: aws-vpcs unchanged"     '"kind":"aws-vpc".*"action":"unchanged"'     2 "$APPLY2"
  assert_count "second apply: aws-subnet unchanged"   '"kind":"aws-subnet".*"action":"unchanged"'  1 "$APPLY2"
}

phase_verify() {
  echo "==> resolving the project's Docker network"
  NETWORK=$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" --format '{{.Name}}' | head -n1)
  [ -n "$NETWORK" ] || { echo "could not find a Docker network for project ${PROJECT}" >&2; exit 1; }
  echo "network: ${NETWORK}"

  echo "==> running the plugin-mode import verification suite"
  docker run --rm --network "$NETWORK" \
    -e IPAM_E2E_PLUGIN_IMPORT=1 \
    -e IPAM_NETBOX_TOKEN="$IPAM_NETBOX_TOKEN" \
    -e IPAM_LOCAL_TOKEN="$IPAM_LOCAL_TOKEN" \
    -v "$ROOT/tests/e2e/plugin_mode_import.py:/plugin_mode_import.py:ro" \
    docker.io/python:3.12-alpine \
    python3 -m unittest /plugin_mode_import.py -v
}

phase_down() {
  echo "==> tearing down project ${PROJECT}"
  compose down -v --remove-orphans || true
  echo "==> containers left in ${PROJECT} (should be empty):"
  docker compose -p "$PROJECT" ps -a 2>/dev/null || true
  echo "==> volumes left for ${PROJECT} (should be empty):"
  docker volume ls | grep "$PROJECT" || echo "(none)"
  rm -f "$WORK_ENV" "$OVERRIDE"
}

phase_all() {
  trap 'phase_down' EXIT INT TERM
  phase_build
  phase_netbox_up
  deadline=$(( $(date +%s) + 1200 ))
  while :; do
    if phase_netbox_wait | tee /dev/stderr | grep -q '^NETBOX_HEALTHY=1$'; then
      break
    fi
    [ "$(date +%s)" -lt "$deadline" ] || { echo "NetBox did not become healthy within the overall deadline" >&2; exit 1; }
  done
  phase_rest_up
  phase_bootstrap
  phase_import
  phase_verify
  echo "==> all plugin-mode import checks passed"
}

phase="${1:-all}"
case "$phase" in
  build)        phase_build ;;
  netbox-up)    phase_netbox_up ;;
  netbox-wait)  phase_netbox_wait ;;
  rest-up)      phase_rest_up ;;
  bootstrap)    phase_bootstrap ;;
  import)       phase_import ;;
  verify)       phase_verify ;;
  down)         phase_down ;;
  all)          phase_all ;;
  *)
    echo "usage: $0 [build|netbox-up|netbox-wait|rest-up|bootstrap|import|verify|down|all]" >&2
    exit 2
    ;;
esac
