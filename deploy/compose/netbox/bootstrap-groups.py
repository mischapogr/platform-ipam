"""Development-only NetBox read-only operator group bootstrap.

Runs inside the NetBox image through `manage.py shell`, alongside
`bootstrap-token.py`, which creates the break-glass superuser and the
platform adapter's own API token. This script instead creates the two
operator-facing groups the [operator UI authentication
design](../../../docs/GUI_AUTHENTICATION.md) calls for:

  * `platform-operators` -- view-only on the IPAM objects an operator needs
    to read the inventory (prefixes, VRFs, aggregates, IP ranges, IP
    addresses) plus the custom-field and tag definitions the UI renders
    alongside them.
  * `platform-inventory-maintainers` -- view, add and change on prefixes and
    IP ranges only. No delete, no VRF/aggregate/address access.

Both are granted through `users.ObjectPermission`, NetBox's row-level
permission model; there is no coarser "read-only" role to assign instead.
Neither group is a NetBox superuser or staff account, so they carry no
access beyond what their ObjectPermission grants.

For the end-to-end suite only (package A2, `tests/e2e/test_e2e_netbox_roles.py`),
this script also creates a development user `e2e-viewer` in
`platform-operators` with a v1 API token, but only when
`NETBOX_E2E_VIEWER_TOKEN` is set -- the same 40-character-plaintext v1 token
shape `bootstrap-token.py` uses for the adapter's own token, for the same
reason: the platform's NetBox adapter (and this test) authenticate with the
legacy `Authorization: Token <secret>` header, not the v2 bearer scheme.

The script is idempotent and refuses to run against anything but the local
development stack.
"""

from os import environ
import sys

from core.models import ObjectType
from users.choices import TokenVersionChoices
from users.models import Group, ObjectPermission, Token, User

TOKEN_PLAINTEXT_LENGTH = 40

# (app_label, model) pairs, lower-cased exactly as NetBox's ContentType /
# ObjectType table stores them (see users.models.ObjectPermission.object_types
# and utilities.permissions.resolve_permission_type in the pinned image).
GROUPS = {
    "platform-operators": {
        "description": "Read-only access to the IPAM inventory for platform operators.",
        "actions": ["view"],
        "object_types": [
            ("ipam", "prefix"),
            ("ipam", "vrf"),
            ("ipam", "aggregate"),
            ("ipam", "iprange"),
            ("ipam", "ipaddress"),
            ("extras", "customfield"),
            ("extras", "customfieldchoiceset"),
            ("extras", "tag"),
        ],
    },
    "platform-inventory-maintainers": {
        "description": "Create and update IPAM prefixes and IP ranges. No other write access.",
        "actions": ["view", "add", "change"],
        "object_types": [
            ("ipam", "prefix"),
            ("ipam", "iprange"),
        ],
    },
}

if environ.get("IPAM_ENVIRONMENT") != "development":
    print("refusing to bootstrap operator groups outside IPAM_ENVIRONMENT=development")
    sys.exit(1)


def object_types_for(pairs):
    resolved = []
    for app_label, model in pairs:
        try:
            resolved.append(ObjectType.objects.get_by_natural_key(app_label=app_label, model=model))
        except ObjectType.DoesNotExist:
            print(f"unknown object type {app_label}.{model}; is this still the pinned NetBox image?")
            sys.exit(1)
    return resolved


def ensure_group(name, description):
    group, created = Group.objects.get_or_create(name=name, defaults={"description": description})
    if not created and group.description != description:
        group.description = description
        group.save()
    print(f"{'created' if created else 'confirmed'} group {name}")
    return group


def ensure_permission(group, actions, object_type_pairs):
    """Create or converge the single ObjectPermission backing `group`.

    One ObjectPermission per group is enough: `actions` and `constraints`
    (left unset, i.e. unrestricted) apply uniformly across every object type
    attached to it, and this group needs only one action set.
    """
    permission_name = f"{group.name}-permissions"
    object_types = object_types_for(object_type_pairs)

    permission, created = ObjectPermission.objects.get_or_create(
        name=permission_name,
        defaults={"actions": list(actions), "enabled": True},
    )
    changed = False
    if not created:
        if sorted(permission.actions) != sorted(actions):
            permission.actions = list(actions)
            changed = True
        if not permission.enabled:
            permission.enabled = True
            changed = True
        if changed:
            permission.save()

    desired_type_ids = {object_type.pk for object_type in object_types}
    current_type_ids = set(permission.object_types.values_list("pk", flat=True))
    if current_type_ids != desired_type_ids:
        permission.object_types.set(object_types)
        changed = True

    if group not in permission.groups.all():
        permission.groups.add(group)
        changed = True

    print(f"{'created' if created else 'confirmed'} object permission {permission_name} "
          f"({'converged' if changed else 'unchanged'})")


for group_name, spec in GROUPS.items():
    ensure_group(group_name, spec["description"])

# Grant the permission after every group exists so `ensure_permission` can be
# re-run safely regardless of dict ordering.
for group_name, spec in GROUPS.items():
    group = Group.objects.get(name=group_name)
    ensure_permission(group, spec["actions"], spec["object_types"])

# --- e2e-only viewer user -----------------------------------------------
#
# Gated on NETBOX_E2E_VIEWER_TOKEN being set (the Compose service requires
# it, so this normally runs) rather than being unconditional, so a bootstrap
# invoked without that variable still converges the groups above on their
# own.
viewer_token_value = environ.get("NETBOX_E2E_VIEWER_TOKEN", "")
if viewer_token_value:
    viewer_username = environ.get("NETBOX_E2E_VIEWER_NAME", "e2e-viewer")
    viewer_email = environ.get("NETBOX_E2E_VIEWER_EMAIL", "e2e-viewer@example.invalid")

    if len(viewer_token_value) != TOKEN_PLAINTEXT_LENGTH:
        # Same 40-character constraint as the adapter's v1 token; fail loudly
        # rather than leaving a test viewer whose token NetBox will reject.
        print(
            f"NETBOX_E2E_VIEWER_TOKEN must be exactly {TOKEN_PLAINTEXT_LENGTH} characters "
            f"for a NetBox v1 token; got {len(viewer_token_value)}"
        )
        sys.exit(1)

    viewer = User.objects.filter(username=viewer_username).first()
    if viewer is None:
        # No password is passed, so Django gives the account an unusable
        # password: this user authenticates only via its API token, never
        # through the login form.
        viewer = User.objects.create_user(viewer_username, viewer_email)
        print(f"created user {viewer_username}")
    else:
        print(f"user {viewer_username} already exists")

    operators = Group.objects.get(name="platform-operators")
    if operators not in viewer.groups.all():
        viewer.groups.add(operators)
        print(f"added {viewer_username} to platform-operators")
    else:
        print(f"{viewer_username} already in platform-operators")

    if Token.objects.filter(plaintext=viewer_token_value).exists():
        print("e2e viewer API token already present")
    else:
        # Remove any prior token for this user so a rotated .env converges
        # instead of accumulating stale credentials, matching bootstrap-token.py.
        removed, _ = Token.objects.filter(user=viewer, version=TokenVersionChoices.V1).delete()
        if removed:
            print(f"removed {removed} superseded e2e viewer token(s)")
        # As in bootstrap-token.py: the value must be assigned through the
        # `token` property, not `plaintext=`, or Token.save() would generate
        # a fresh random value instead of using the one the test expects.
        Token.objects.create(
            user=viewer,
            version=TokenVersionChoices.V1,
            token=viewer_token_value,
            description="e2e read-only viewer (test only)",
            write_enabled=False,
        )
        print("created e2e viewer API token")
else:
    print("NETBOX_E2E_VIEWER_TOKEN not set; skipping e2e viewer bootstrap")
