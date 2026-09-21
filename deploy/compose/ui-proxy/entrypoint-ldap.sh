#!/bin/sh
# ldap-mode entrypoint for ui-proxy (package A5). Unlike entrypoint.sh
# (`basic` mode), this proxy never authenticates anyone itself -- NetBox's
# own LDAP backend does (compose.ui-ldap.yaml's AUTH_LDAP_* settings on the
# netbox service) -- so there is no bcrypt hash to compute and
# NETBOX_UI_USER/NETBOX_UI_PASSWORD are never read here, even though the
# base compose.netbox.yaml service definition still requires
# NETBOX_UI_PASSWORD to be set in the environment (it is simply unused in
# this mode). Caddyfile.ldap has no basic_auth directive to feed a hash to.
set -eu

exec caddy run --config /etc/caddy/Caddyfile.ldap --adapter caddyfile
