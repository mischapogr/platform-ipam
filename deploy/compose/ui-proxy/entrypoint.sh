#!/bin/sh
# Computes the ui-proxy basic-auth credential's bcrypt hash at container
# start, from the plaintext NETBOX_UI_PASSWORD that deploy/compose/.env
# carries (create-env.sh generates it; see that file and .env.example).
#
# Why here rather than baking the hash into create-env.sh: create-env.sh is a
# POSIX shell script that deliberately has no Docker dependency -- it only
# needs `openssl` or `/dev/urandom` -- and a `.env` can be created before the
# stack (or Docker) is available at all. Hashing here instead mirrors how
# NETBOX_SUPERUSER_PASSWORD is already handled: create-env.sh writes only a
# plaintext development secret, and a container converts it at start (there,
# Django's set_password in bootstrap-token.py; here, `caddy hash-password`).
# It also means a rotated NETBOX_UI_PASSWORD in .env takes effect on the next
# `up`/recreate of this service with no separate hash to regenerate or keep in
# sync by hand, and the hash is never written to disk or logged.
set -eu

: "${NETBOX_UI_USER:?NETBOX_UI_USER must be set}"
: "${NETBOX_UI_PASSWORD:?NETBOX_UI_PASSWORD must be set}"

# `caddy hash-password` writes the hash and a trailing newline to stdout;
# $() strips it. The Caddyfile substitutes {$NETBOX_UI_PASSWORD_HASH} as a
# plain string, so the hash's own `$`-delimited bcrypt fields need no escaping
# here.
NETBOX_UI_PASSWORD_HASH=$(caddy hash-password --plaintext "$NETBOX_UI_PASSWORD" --algorithm bcrypt)
export NETBOX_UI_PASSWORD_HASH

exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
