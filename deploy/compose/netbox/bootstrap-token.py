"""Development-only NetBox credential bootstrap.

Runs inside the NetBox image through `manage.py shell`, because a NetBox API
token can only be created from NetBox's own Django environment. The image's
built-in `super_user.py` is not usable here: it creates a v2 token
(`Authorization: Bearer nbt_<key>.<secret>`), while the platform's NetBox
adapter sends the legacy v1 header (`Authorization: Token <secret>`). This
script therefore creates a v1 token, whose plaintext NetBox constrains to
exactly 40 characters.

The script is idempotent and refuses to run against anything but the local
development stack.
"""

from os import environ
import sys

from django.conf import settings
from users.choices import TokenVersionChoices
from users.models import Token, User

TOKEN_PLAINTEXT_LENGTH = 40

username = environ.get("NETBOX_SUPERUSER_NAME", "admin")
email = environ.get("NETBOX_SUPERUSER_EMAIL", "admin@example.invalid")
password = environ.get("NETBOX_SUPERUSER_PASSWORD", "")
token_value = environ.get("IPAM_NETBOX_TOKEN", "")

if environ.get("IPAM_ENVIRONMENT") != "development":
    print("refusing to bootstrap credentials outside IPAM_ENVIRONMENT=development")
    sys.exit(1)
if not password:
    print("NETBOX_SUPERUSER_PASSWORD is required")
    sys.exit(1)
if len(token_value) != TOKEN_PLAINTEXT_LENGTH:
    # A v1 token is validated at exactly 40 characters. Fail loudly rather
    # than leaving a stack whose adapter cannot authenticate.
    print(
        f"IPAM_NETBOX_TOKEN must be exactly {TOKEN_PLAINTEXT_LENGTH} characters "
        f"for a NetBox v1 token; got {len(token_value)}"
    )
    sys.exit(1)

user = User.objects.filter(username=username).first()
if user is None:
    user = User.objects.create_superuser(username, email, password)
    print(f"created superuser {username}")
else:
    print(f"superuser {username} already exists")

if Token.objects.filter(plaintext=token_value).exists():
    print("development API token already present")
    sys.exit(0)

# Remove any prior development token for this user so a rotated .env converges
# instead of accumulating stale credentials.
removed, _ = Token.objects.filter(user=user, version=TokenVersionChoices.V1).delete()
if removed:
    print(f"removed {removed} superseded development token(s)")

# The value must be assigned through the `token` property, not `plaintext`:
# Token.save() generates a fresh random value whenever `token` was never set,
# which would silently replace the credential the adapter is configured with.
Token.objects.create(
    user=user,
    version=TokenVersionChoices.V1,
    token=token_value,
    description="platform-ipam local development",
    write_enabled=True,
)
print("created development API token")
if not settings.API_TOKEN_PEPPERS:
    print("note: API_TOKEN_PEPPERS is unset; v2 tokens would not be creatable")
