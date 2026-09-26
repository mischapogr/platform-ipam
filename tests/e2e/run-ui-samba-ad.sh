#!/usr/bin/env sh
# Opt-in Samba AD protocol gate. Uses an isolated Compose project and retains
# volumes on stop, so a failed run can be inspected and restarted.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$ROOT/deploy/compose"
[ -f .env ] || { echo "deploy/compose/.env is missing" >&2; exit 2; }
PROJECT=platform-ipam-ls1b
WORK_ENV="${TMPDIR:-/tmp}/${PROJECT}.env"
cp .env "$WORK_ENV"
sed -i -e "s/^COMPOSE_PROJECT_NAME=.*/COMPOSE_PROJECT_NAME=${PROJECT}/" \
  -e 's/^NETBOX_PORT=.*/NETBOX_PORT=18087/' "$WORK_ENV"
set -a
# shellcheck disable=SC1090
. "$WORK_ENV"
set +a
[ "${IPAM_ENVIRONMENT:-}" = development ] || { echo "development environment required" >&2; exit 2; }

compose() {
  docker compose --env-file "$WORK_ENV" -p "$PROJECT" \
    -f compose.yaml -f compose.netbox.yaml -f compose.ui-samba-ad.yaml "$@"
}

case "${1:-}" in
  up)
    compose config -q
    compose up -d samba-ad netbox-db netbox-redis netbox-redis-cache netbox
    ;;
  wait)
    deadline=$(( $(date +%s) + 540 ))
    while :; do
      health=$(compose ps --format '{{.Health}}' netbox 2>/dev/null || true)
      [ "$health" = healthy ] && { echo HEALTHY; exit 0; }
      [ "$(date +%s)" -ge "$deadline" ] && { echo "netbox health: ${health:-unknown}"; exit 3; }
      sleep 5
    done
    ;;
  bootstrap)
    [ "$(compose ps --format '{{.Health}}' netbox)" = healthy ] || { echo "wait for NetBox health first" >&2; exit 3; }
    if ! compose exec -T samba-ad samba-tool group listmembers platform-operators | grep -Fxq nested-viewers; then
      compose exec -T samba-ad samba-tool group addmembers platform-operators nested-viewers
    fi
    compose up -d ui-proxy
    compose --profile bootstrap run --rm netbox-bootstrap
    ;;
  test)
    network=$(docker network ls --filter "label=com.docker.compose.project=${PROJECT}" --format '{{.Name}}' | head -n1)
    [ -n "$network" ] || { echo "isolated network missing" >&2; exit 3; }
    docker run --rm --network "$network" \
      -e IPAM_E2E_LDAP=1 \
      -e NETBOX_UI_LDAP_BASE_URL=http://ui-proxy:8080 \
      -e NETBOX_INTERNAL_URL=http://netbox:8080 \
      -e NETBOX_UI_LDAP_VIEWER_PASSWORD=LocalViewerPassw0rd! \
      -e NETBOX_UI_LDAP_MAINTAINER_PASSWORD=LocalMaintainerPassw0rd! \
      -e NETBOX_UI_LDAP_OUTSIDER_PASSWORD=LocalOutsiderPassw0rd! \
      -e NETBOX_UI_LDAP_EXTRA_GROUP=nested-viewers \
      -e IPAM_NETBOX_TOKEN="$IPAM_NETBOX_TOKEN" \
      -v "$ROOT/tests/e2e/ui_mode_ldap.py:/ui_mode_ldap.py:ro" \
      docker.io/python:3.12-alpine python3 -m unittest /ui_mode_ldap.py -v
    compose exec -T netbox /opt/netbox/venv/bin/python /opt/netbox/netbox/manage.py shell --no-startup --interface python <<'PY'
from users.models import User
for name in ('viewer', 'maintainer'):
    user = User.objects.get(username=name)
    assert not user.is_superuser, name
print('AD users have no superuser grant')
PY
    compose exec -T netbox /opt/netbox/venv/bin/python - <<'PY'
import ldap
import os

url = os.environ['AUTH_LDAP_SERVER_URI']
bind_dn = os.environ['AUTH_LDAP_BIND_DN']
password = os.environ['AUTH_LDAP_BIND_PASSWORD']
base = os.environ['AUTH_LDAP_USER_SEARCH_BASEDN']
ldap.set_option(ldap.OPT_X_TLS_CACERTFILE, os.environ['LDAP_CA_CERT_FILE'])
ldap.set_option(ldap.OPT_X_TLS_REQUIRE_CERT, ldap.OPT_X_TLS_DEMAND)
connection = ldap.initialize(url)
connection.simple_bind_s(bind_dn, password)
matches = connection.search_s(base, ldap.SCOPE_SUBTREE, '(sAMAccountName=viewer)', ['sAMAccountName'])
assert len(matches) == 1, matches
print('trusted LDAPS and sAMAccountName lookup passed')
PY
    compose exec -T netbox /opt/netbox/venv/bin/python - <<'PY'
import ldap
import os

ldap.set_option(ldap.OPT_X_TLS_CACERTFILE, '/etc/ssl/certs/ca-certificates.crt')
ldap.set_option(ldap.OPT_X_TLS_REQUIRE_CERT, ldap.OPT_X_TLS_DEMAND)
connection = ldap.initialize(os.environ['AUTH_LDAP_SERVER_URI'])
try:
    connection.simple_bind_s(os.environ['AUTH_LDAP_BIND_DN'], os.environ['AUTH_LDAP_BIND_PASSWORD'])
except ldap.SERVER_DOWN:
    print('untrusted LDAPS refused')
else:
    raise AssertionError('untrusted LDAPS was accepted')
PY
    ;;
  stop)
    compose stop
    ;;
  *)
    echo "usage: $0 {up|wait|bootstrap|test|stop}" >&2
    exit 2
    ;;
esac
