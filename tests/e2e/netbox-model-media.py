"""Disposable NetBox attachment fixture; executed through manage.py shell only.

The runner supplies ACTION. Credentials stay inside the private NetBox database.
HTTP probes authenticate real API requests; they do not authenticate a browser.
"""

import hashlib
import io
import json
import secrets
import urllib.error
import urllib.request

from django.core.files.base import ContentFile
from django.core.serializers.json import DjangoJSONEncoder
from core.models import ObjectType
from dcim.models import Site
from extras.models import ImageAttachment
from PIL import Image
from users.choices import TokenVersionChoices
from users.models import ObjectPermission, Token, User


def image_bytes(color):
    output = io.BytesIO()
    Image.new("RGB", (2, 2), color).save(output, format="PNG")
    return output.getvalue()


def seed():
    for label, color in (("a", "red"), ("b", "blue")):
        site = Site.objects.create(name=f"Recovery site {label}", slug=f"recovery-site-{label}")
        attachment = ImageAttachment(parent=site, name=f"recovery-image-{label}", description="Synthetic recovery witness")
        attachment.image.save(f"recovery-{label}.png", ContentFile(image_bytes(color)), save=False)
        attachment.full_clean()
        attachment.save()
        user = User.objects.create_user(username=f"recovery-reader-{label}")
        user.set_unusable_password()
        user.save()
        Token.objects.create(user=user, version=TokenVersionChoices.V1, token=secrets.token_hex(20),
                             write_enabled=True, description="Disposable recovery API probe")
        for model, pk in ((Site, site.pk), (ImageAttachment, attachment.pk)):
            permission = ObjectPermission.objects.create(name=f"recovery-{label}-{model._meta.model_name}",
                                                        actions=["view"], constraints={"id": pk})
            permission.object_types.add(ObjectType.objects.get_for_model(model))
            permission.users.add(user)
    return snapshot()


def snapshot():
    records = []
    for label, color in (("a", "red"), ("b", "blue")):
        attachment = ImageAttachment.objects.get(name=f"recovery-image-{label}")
        site = Site.objects.get(slug=f"recovery-site-{label}")
        if attachment.parent != site:
            raise AssertionError("Attachment parent differs from its fixture site")
        with attachment.image.open("rb") as image:
            data = image.read()
        if data != image_bytes(color) or (attachment.image_width, attachment.image_height) != (2, 2):
            raise AssertionError("Attachment bytes or dimensions differ")
        user = User.objects.get(username=f"recovery-reader-{label}")
        if user.is_superuser or user.has_usable_password():
            raise AssertionError("Fixture reader gained elevated or browser credentials")
        records.append({"attachment": ImageAttachment.objects.filter(pk=attachment.pk).values().get(),
                        "site": Site.objects.filter(pk=site.pk).values().get(),
                        "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data),
                        "reader_id": user.pk,
                        "permissions": list(ObjectPermission.objects.filter(users=user).order_by("pk")
                                            .values("id", "name", "actions", "constraints", "enabled"))})
    return records


def probe():
    def request(path, token=None, method="GET"):
        headers = {"Authorization": "Token " + token} if token else {}
        data = b'{"description":"forbidden mutation"}' if method == "PATCH" else None
        if data:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request("http://netbox:8080/api/" + path, headers=headers, data=data, method=method)
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            return error.code, None

    statuses = {}
    for label, other in (("a", "b"), ("b", "a")):
        user = User.objects.get(username=f"recovery-reader-{label}")
        token = Token.objects.get(user=user).plaintext
        own = ImageAttachment.objects.get(name=f"recovery-image-{label}")
        foreign = ImageAttachment.objects.get(name=f"recovery-image-{other}")
        path = f"extras/image-attachments/{own.pk}/"
        own_status, body = request(path, token)
        foreign_status, _ = request(f"extras/image-attachments/{foreign.pk}/", token)
        anonymous_status, _ = request(path)
        write_status, _ = request(path, token, "PATCH")
        list_status, listing = request("extras/image-attachments/", token)
        if (own_status, foreign_status, write_status, list_status) != (200, 404, 403, 200):
            raise AssertionError("Attachment API object authorization differs")
        if anonymous_status not in (401, 403) or {row["id"] for row in listing["results"]} != {own.pk}:
            raise AssertionError("Attachment API exposed an unauthorized record")
        if body["object_id"] != own.parent.pk or body["object_type"] != "dcim.site":
            raise AssertionError("Attachment API parent differs")
        statuses[label] = {"own_get": own_status, "foreign_get": foreign_status,
                           "anonymous_get": anonymous_status, "patch": write_status,
                           "list_count": len(listing["results"])}
    return statuses


def negative():
    attachment = ImageAttachment.objects.get(name="recovery-image-a")
    path = attachment.image.path
    from pathlib import Path
    original = Path(path)
    hidden = original.with_suffix(".withheld")
    original.rename(hidden)
    try:
        try:
            snapshot()
        except FileNotFoundError:
            pass
        else:
            raise AssertionError("Missing model media was admitted")
    finally:
        hidden.rename(original)
    parent_id = attachment.object_id
    ImageAttachment.objects.filter(pk=attachment.pk).update(object_id=Site.objects.get(slug="recovery-site-b").pk)
    try:
        try:
            snapshot()
        except AssertionError as error:
            if str(error) != "Attachment parent differs from its fixture site":
                raise
        else:
            raise AssertionError("Wrong attachment parent was admitted")
    finally:
        ImageAttachment.objects.filter(pk=attachment.pk).update(object_id=parent_id)
    snapshot()
    return ["missing_media_refused_by_runner", "wrong_attachment_parent_refused_by_runner"]


try:
    result = {"seed": seed, "snapshot": snapshot, "probe": probe, "negative": negative}[ACTION]()
except Exception as error:
    # Report setup/validation errors without retaining credential-shaped strings.
    import re
    result = {"fixture_error": type(error).__name__ + ": " + re.sub(r"[A-Za-z0-9_-]{32,}", "[redacted]", str(error))}
print(json.dumps(result, cls=DjangoJSONEncoder, sort_keys=True))
