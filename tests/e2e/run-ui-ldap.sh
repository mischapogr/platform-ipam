#!/usr/bin/env sh
# `ldap` mode end-to-end runner (package A5, docs/GUI_AUTHENTICATION.md "LDAP
# mode (package A5)", ADR 0006).
#
# Unlike run-e2e.sh, this script never touches the shared development
# project (platform-ipam-dev): it brings up deploy/compose/compose.ui-ldap.yaml
# in its OWN throw-away Compose project, starting only the NetBox-side
# services the overlay needs (never the platform api/worker, which already
# publish the host's port 8080 in the main project), and tears that project
# down -- containers and volumes -- when it finishes, pass or fail.
#
# It never assumes a host loopback port is reachable from wherever it runs:
# the test itself (tests/e2e/ui_mode_ldap.py) is executed inside a
# throw-away container attached to the project's own Docker network, talking
# to `ui-proxy` and `netbox` by their Compose service names, the same way the
# platform adapter does.
#
# ui_mode_ldap.py is named the way it is -- not test_e2e_ui_ldap.py --
# specifically so run-e2e.sh's `test_e2e_*.py` glob never sweeps it into a
# run against the shared platform-ipam-dev stack (which runs `basic` mode,
# not `ldap`); it is invoked here directly, by path.
#
# Phased, not one long blocking script: a from-scratch NetBox's first-run
# migrations can take well over ten minutes, longer than a single command
# invocation is often allowed to block for in an automated environment.
# `up` returns in seconds; `wait` polls for a few minutes and then returns
# (0 once healthy, 3 if still not -- re-run `wait` until it prints HEALTHY);
# `bootstrap`, `test` and `down` are each one bounded step. `all` (the
# default, and the one a human with no such limit should use) just runs
# every phase in order, itself looping `wait` until healthy, and always
# tears the project down at the end, pass or fail.
#
# NOTE on `up` NOT including ui-proxy: ui-proxy `depends_on: netbox:
# condition: service_healthy`. Docker Compose's own `up -d` blocks on that
# condition using netbox's healthcheck's own interval/retries budget
# (roughly 4-5 minutes with this overlay's settings) and ABORTS THE WHOLE
# `up -d` CALL -- including the other, unrelated services it was asked to
# start -- if netbox is still not healthy when that budget runs out. A cold
# migration reliably takes longer than that, so asking for ui-proxy in the
# same `up -d` as netbox fails hard well before NetBox is actually done
# (observed directly while building this script). `up` therefore starts
# only the services with no such dependency; `bootstrap` brings up ui-proxy
# in its own call, only after this script's own longer `wait` loop has
# confirmed netbox is healthy.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
COMPOSE_DIR="$ROOT/deploy/compose"
cd "$COMPOSE_DIR"

[ -f .env ] || { echo "deploy/compose/.env is missing; run ./create-env.sh" >&2; exit 2; }

case "${IPAM_NETBOX_CANDIDATE:-0}" in
  0) PROJECT=platform-ipam-a5; UI_PORT=18085 ;;
  1) PROJECT=platform-ipam-a5-4610; UI_PORT=18105 ;;
  *) echo "IPAM_NETBOX_CANDIDATE must be 0 or 1" >&2; exit 2 ;;
esac
# A fixed path (not mktemp-random): phases run as separate process
# invocations and need to agree on it, and rebuilding it fresh at the start
# of every phase (below) means it always reflects the current
# deploy/compose/.env, never a stale copy from an earlier phase.
WORK_ENV="${TMPDIR:-/tmp}/${PROJECT}.env"

build_work_env() {
  # A dedicated copy of .env with only the two values this isolation scheme
  # needs changed: the project name (so `down -v` can never reach the shared
  # platform-ipam-dev project's containers/volumes) and the published port
  # (platform-ipam-dev's ui-proxy already holds 18000/NETBOX_PORT's default).
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
}
build_work_env

# shellcheck disable=SC1090
set -a; . "$WORK_ENV"; set +a
: "${IPAM_ENVIRONMENT:?IPAM_ENVIRONMENT must be set}"
if [ "$IPAM_ENVIRONMENT" != "development" ]; then
  echo "refusing to run against a non-development environment" >&2
  exit 2
fi

compose() {
  if [ "${IPAM_NETBOX_CANDIDATE:-0}" = 1 ]; then
    docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
      -f compose.yaml -f compose.netbox.yaml -f compose.ui-ldap.yaml \
      -f compose.netbox-compat-4_6_10.yaml "$@"
  else
    docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
      -f compose.yaml -f compose.netbox.yaml -f compose.ui-ldap.yaml "$@"
  fi
}

phase_up() {
  echo "==> validating the overlay"
  compose config -q
  echo "==> starting NetBox-side services and the LDAP test directory (ui-proxy not yet -- see the file header)"
  compose up -d netbox-db netbox-redis netbox-redis-cache netbox openldap
}

phase_wait() {
  # One bounded call: ~9 minutes, then returns so the caller can re-invoke
  # this phase instead of blocking past whatever time limit called it.
  echo "==> polling for NetBox health (re-run this phase until it prints HEALTHY; first-run migrations take 5-15 minutes)"
  deadline=$(( $(date +%s) + 540 ))
  while :; do
    health=$(compose ps --format '{{.Health}}' netbox 2>/dev/null || echo "")
    if [ "$health" = "healthy" ]; then
      echo "HEALTHY"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "still ${health:-unknown} after this call's budget; recent netbox log:"
      compose logs --tail 5 netbox || true
      return 3
    fi
    sleep 5
  done
}

phase_bootstrap() {
  echo "==> bringing up ui-proxy now that NetBox is confirmed healthy"
  compose up -d ui-proxy
  echo "==> bootstrapping NetBox credentials and the operator groups"
  compose --profile bootstrap run --rm netbox-bootstrap
}

phase_test() {
  echo "==> resolving the project's Docker network"
  network=$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" --format '{{.Name}}' | head -n1)
  [ -n "$network" ] || { echo "could not find a Docker network for project ${PROJECT}" >&2; exit 1; }
  echo "network: ${network}"

  echo "==> running the ldap-mode security tests"
  docker run --rm --network "$network" \
    -e IPAM_E2E_LDAP=1 \
    -e NETBOX_UI_LDAP_BASE_URL=http://ui-proxy:8080 \
    -e NETBOX_INTERNAL_URL=http://netbox:8080 \
    -e IPAM_NETBOX_TOKEN="$IPAM_NETBOX_TOKEN" \
    -v "$ROOT/tests/e2e/ui_mode_ldap.py:/ui_mode_ldap.py:ro" \
    docker.io/python:3.12-alpine \
    python3 -m unittest /ui_mode_ldap.py -v

  # is_superuser is checked here, through NetBox's own ORM, because neither
  # this NetBox version's REST UserSerializer nor its GraphQL UserType
  # exposes it (confirmed empirically -- see ui_mode_ldap.py's docstring),
  # so the HTTP-only suite above cannot see it. By this point `viewer` and
  # `maintainer` have both logged in at least once (the suite above did
  # that), so LDAPBackend has already auto-created/updated both NetBox
  # users.
  #
  # is_staff is NOT checked: this NetBox version's User model is
  # `AbstractBaseUser, PermissionsMixin` (users/models/users.py), not
  # Django's `AbstractUser` -- confirmed empirically (`user.is_staff` raises
  # AttributeError; the field does not exist). The pinned image's
  # ldap_config.py still fills an `is_staff` key into
  # AUTH_LDAP_USER_FLAGS_BY_GROUP whenever AUTH_LDAP_REQUIRE_GROUP_DN is
  # set, but django-auth-ldap setting a nonexistent model field is a
  # harmless dynamic Python attribute, never persisted and never consulted
  # by anything -- is_superuser is the only elevation flag this NetBox
  # version has.
  echo "==> verifying viewer/maintainer are not superusers (via NetBox's ORM)"
  compose exec -T netbox /opt/netbox/venv/bin/python /opt/netbox/netbox/manage.py shell --no-startup --interface python <<'PY'
import sys
from users.models import User

failures = []
for username in ("viewer", "maintainer"):
    user = User.objects.filter(username=username).first()
    if user is None:
        failures.append(f"{username}: no NetBox user found (login should have created it)")
        continue
    if user.is_superuser:
        failures.append(f"{username}: is_superuser is True")
    print(f"{username}: is_superuser={user.is_superuser}")

if failures:
    print("FAILURES:")
    for line in failures:
        print(" -", line)
    sys.exit(1)
print("OK: no LDAP group granted superuser")
PY
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
  up) phase_up ;;
  wait) phase_wait ;;
  bootstrap) phase_bootstrap ;;
  test) phase_test ;;
  stop) phase_stop ;;
  down) phase_down ;;
  all)
    # The one-shot path for a human, or CI, with no per-call time limit.
    # Always tears the project down, pass or fail.
    trap phase_down EXIT INT TERM
    phase_up
    until phase_wait; do :; done
    phase_bootstrap
    phase_test
    ;;
  *)
    echo "usage: $0 [up|wait|bootstrap|test|stop|down|all]" >&2
    echo "  (no argument = all: the one-shot path, for a human or CI with no per-call time limit)" >&2
    exit 2
    ;;
esac
