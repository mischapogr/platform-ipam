#!/usr/bin/env sh
# End-to-end suite runner (ADR 0002).
#
# Brings the development stack to a known allocation state and runs the suite
# against it. Allocation keys are permanent and quarantine holds are deliberate,
# so a suite that only appended would exhaust the pool's quota after a few
# runs. This script therefore resets the *allocation* state -- the platform
# ledger and the managed NetBox prefixes -- while keeping NetBox's own
# database, whose migrations take minutes.
#
# The reset is destructive. It refuses to run unless the environment is
# development and the Compose project is the disposable development one.
#
# Two opt-in switches let one verification be split across two invocations,
# because an agent's foreground shell command is capped at about ten minutes
# and the build, reset and tests together now need most of that
# (docs/WORK_PLAN.md, the trap in section 2). With neither set, this script
# behaves exactly as it always has, which is what CI invokes:
#
#   IPAM_E2E_SKIP_PREPARE=1  run only the tests, against a stack this script
#                            has already built, bootstrapped and reset. It
#                            skips the build, the health wait, the bootstrap
#                            and the destructive reset, so the ledger still
#                            holds whatever the earlier half put there --
#                            allocation keys are permanent and quota is
#                            finite, so never run the same module twice this
#                            way.
#   IPAM_E2E_PATTERN=<glob>  the unittest discovery pattern, default
#                            'test_e2e_*.py'. Extra arguments are still passed
#                            through to `unittest discover`.
#
# Export the same IPAM_E2E_RUN_ID across both halves: it prefixes every
# allocation key and seeds the per-module address-slot rotation, so two halves
# under different run ids can pick the same CIDRs.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE_DIR="$ROOT/deploy/compose"
cd "$COMPOSE_DIR"

[ -f .env ] || { echo "deploy/compose/.env is missing; run ./create-env.sh" >&2; exit 2; }
# shellcheck disable=SC1091
requested_project=${COMPOSE_PROJECT_NAME:-}
set -a; . ./.env; set +a
if [ -n "$requested_project" ]; then
  COMPOSE_PROJECT_NAME=$requested_project
  export COMPOSE_PROJECT_NAME
fi

: "${IPAM_ENVIRONMENT:?IPAM_ENVIRONMENT must be set}"
: "${COMPOSE_PROJECT_NAME:=platform-ipam-dev}"
if [ "$IPAM_ENVIRONMENT" != "development" ]; then
  echo "refusing to run the destructive e2e suite outside development" >&2
  exit 2
fi
case "$COMPOSE_PROJECT_NAME" in
  *dev*|*test*|*e2e*) ;;
  *) echo "refusing to reset Compose project '$COMPOSE_PROJECT_NAME'" >&2; exit 2 ;;
esac

compose() {
  if [ -n "${IPAM_E2E_COMPOSE_OVERLAY:-}" ]; then
    docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml -f "$IPAM_E2E_COMPOSE_OVERLAY" "$@"
  else
    docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml "$@"
  fi
}

RUN_ID=${IPAM_E2E_RUN_ID:-$(date -u +%Y%m%d%H%M%S)}
export IPAM_E2E_RUN_ID="$RUN_ID"
PATTERN=${IPAM_E2E_PATTERN:-test_e2e_*.py}
echo "e2e run ${RUN_ID} (project ${COMPOSE_PROJECT_NAME})"

if [ -n "${IPAM_E2E_SKIP_PREPARE:-}" ]; then
  echo "==> skipping build, bootstrap and reset (IPAM_E2E_SKIP_PREPARE is set)"
  echo "==> running the suite (pattern ${PATTERN})"
  cd "$ROOT"
  exec python3 -m unittest discover -s tests/e2e -p "$PATTERN" -v "$@"
fi

echo "==> building and starting the stack"
compose up --build -d

echo "==> waiting for NetBox"
deadline=$(( $(date +%s) + 900 ))
until [ "$(compose ps --format '{{.Health}}' netbox 2>/dev/null)" = "healthy" ]; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "NetBox did not become healthy" >&2; exit 1; }
  sleep 5
done

echo "==> bootstrapping NetBox credentials and inventory"
compose --profile bootstrap run --rm netbox-bootstrap >/dev/null
compose --profile bootstrap run --rm seed >/dev/null

echo "==> resetting allocation state"
# Stop the writers first so nothing recreates a row mid-reset.
compose stop api worker >/dev/null
# Delete every NetBox prefix the platform manages. The pool container has no
# allocation marker and is deliberately preserved.
compose --profile bootstrap run --rm -T \
  -e NETBOX_URL="${IPAM_NETBOX_URL:-http://netbox:8080}" \
  -e NETBOX_TOKEN="$IPAM_NETBOX_TOKEN" \
  seed python -c '
import json, os, urllib.request
base = os.environ["NETBOX_URL"].rstrip("/")
token = os.environ["NETBOX_TOKEN"]

def call(path, method="GET", payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(base + path, data=body, method=method)
    request.add_header("Authorization", "Token " + token)
    request.add_header("Content-Type", "application/json")
    request.add_header("Accept", "application/json")
    with urllib.request.urlopen(request, timeout=30) as response:
        raw = response.read()
        return json.loads(raw) if raw.strip() else None

IMPORTED = "platform-ipam-imported"

def tagged(row):
    return any(t.get("slug") == IMPORTED for t in (row.get("tags") or []))

rows = call("/api/ipam/prefixes/?limit=500")["results"]
managed = [r["id"] for r in rows if (r.get("custom_fields") or {}).get("platform_allocation_id")]
# Occupancy written by `onboard apply` carries no allocation marker, so the
# line above never sees it. Left behind, every import test would shrink the
# pool for all later runs. The pool container has neither marker nor tag.
imported = [r["id"] for r in rows if tagged(r) and r["id"] not in managed]
if managed or imported:
    call("/api/ipam/prefixes/", "DELETE", [{"id": i} for i in managed + imported])
ranges = [r["id"] for r in call("/api/ipam/ip-ranges/?limit=500")["results"] if tagged(r)]
if ranges:
    call("/api/ipam/ip-ranges/", "DELETE", [{"id": i} for i in ranges])
print("removed %d managed prefix(es), %d imported prefix(es), %d imported range(s)"
      % (len(managed), len(imported), len(ranges)))
'
# Truncate the ledger. The schema stays, so no migration is needed; only the
# allocation, operation, hold, idempotency, audit and observation rows go.
compose exec -T platform-db psql -q -v ON_ERROR_STOP=1 \
  -U "${PLATFORM_DB_USER:-platform_ipam}" -d "${PLATFORM_DB_NAME:-platform_ipam}" -c "
DO \$\$
DECLARE statement text;
BEGIN
  SELECT string_agg(format('%I.%I', schemaname, tablename), ', ')
    INTO statement
    FROM pg_tables
   WHERE schemaname = 'public'
     AND tablename <> 'schema_migrations';
  IF statement IS NOT NULL THEN
    EXECUTE 'TRUNCATE TABLE ' || statement || ' RESTART IDENTITY CASCADE';
  END IF;
END
\$\$;"

compose up -d api worker >/dev/null
echo "==> waiting for the API"
deadline=$(( $(date +%s) + 180 ))
until compose exec -T api curl -fsS -m 5 http://127.0.0.1:8080/readyz >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || { echo "the API did not become ready" >&2; exit 1; }
  sleep 2
done

echo "==> running the suite (pattern ${PATTERN})"
cd "$ROOT"
exec python3 -m unittest discover -s tests/e2e -p "$PATTERN" -v "$@"
