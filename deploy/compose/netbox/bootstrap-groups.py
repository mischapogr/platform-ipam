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
this script creates development users `e2e-viewer` and `e2e-maintainer` when
their respective token variables are set. Both use the 40-character v1 token
shape `bootstrap-token.py` uses for the adapter: the test authenticates with
the legacy `Authorization: Token <secret>` header, not the v2 bearer scheme.

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

def ensure_e2e_user(token_name, username, email, group_name, write_enabled):
    """Converge a development test user's group and v1 API token."""
    token_value = environ.get(token_name, "")
    if not token_value:
        print(f"{token_name} not set; skipping {username} bootstrap")
        return
    if len(token_value) != TOKEN_PLAINTEXT_LENGTH:
        print(
            f"{token_name} must be exactly {TOKEN_PLAINTEXT_LENGTH} characters "
            f"for a NetBox v1 token; got {len(token_value)}"
        )
        sys.exit(1)

    user = User.objects.filter(username=username).first()
    if user is None:
        # No password is passed: these accounts authenticate only by API token.
        user = User.objects.create_user(username, email)
        print(f"created user {username}")
    else:
        print(f"user {username} already exists")

    group = Group.objects.get(name=group_name)
    if group not in user.groups.all():
        user.groups.add(group)
        print(f"added {username} to {group_name}")

    existing = Token.objects.filter(plaintext=token_value).first()
    if existing is not None:
        if existing.user_id != user.pk or existing.version != TokenVersionChoices.V1:
            print(f"{token_name} belongs to another user or token version")
            sys.exit(1)
        if existing.write_enabled != write_enabled:
            existing.write_enabled = write_enabled
            existing.save(update_fields=["write_enabled"])
        print(f"confirmed e2e token for {username}")
        return

    # A rotated .env replaces the old v1 token instead of keeping stale access.
    removed, _ = Token.objects.filter(user=user, version=TokenVersionChoices.V1).delete()
    if removed:
        print(f"removed {removed} superseded token(s) for {username}")
    # Assign through `token`, not `plaintext=`, so save() uses this value.
    Token.objects.create(
        user=user,
        version=TokenVersionChoices.V1,
        token=token_value,
        description=f"e2e {group_name} (test only)",
        write_enabled=write_enabled,
    )
    print(f"created e2e token for {username}")


ensure_e2e_user(
    "NETBOX_E2E_VIEWER_TOKEN",
    environ.get("NETBOX_E2E_VIEWER_NAME", "e2e-viewer"),
    environ.get("NETBOX_E2E_VIEWER_EMAIL", "e2e-viewer@example.invalid"),
    "platform-operators", False,
)
ensure_e2e_user(
    "NETBOX_E2E_MAINTAINER_TOKEN", "e2e-maintainer",
    "e2e-maintainer@example.invalid", "platform-inventory-maintainers", True,
)
