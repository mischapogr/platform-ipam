# Optional overlay image: the pinned NetBox release plus netbox-aws-vpc-plugin.
#
# Package N2 (docs/WORK_PLAN.md Track N; evaluation: docs/NETBOX_AWS_PLUGIN.md).
# This image is built only when deploy/compose/compose.netbox-plugin.yaml is
# layered on top of compose.netbox.yaml; without that overlay nothing here is
# used and the stack runs the unmodified netboxcommunity/netbox image.
#
# The base image already sets VIRTUAL_ENV=/opt/netbox/venv and puts
# /opt/netbox/venv/bin first on PATH (docker.io/netboxcommunity/netbox
# v4.6.7-5.0.2, netbox-docker release 5.0.2), and ships `uv` for package
# installs -- there is no `pip` binary in the image. `uv pip install` below
# therefore installs straight into the existing venv, which is how
# netbox-docker's own plugin documentation installs third-party plugins for
# this image generation.
ARG NETBOX_BASE_IMAGE=docker.io/netboxcommunity/netbox:v4.6.7-5.0.2
FROM ${NETBOX_BASE_IMAGE}

# Pinned to an exact version per docs/WORK_PLAN.md Track N ("the plugin
# pinned to an exact version"). docs/NETBOX_AWS_PLUGIN.md recommends this
# plugin and this release: PyPI release 0.1.0 (2026-01-20), Apache-2.0,
# min_version="4.5.0" with no max_version declared -- see that doc's
# "Reviewer verification" section for why "supports 4.6.7" is a metadata
# claim only, and why this Dockerfile's build (and the migration check run
# against it) is the real verification.
RUN uv pip install --no-cache netbox-aws-vpc-plugin==0.1.0

# plugins.py is mounted at /etc/netbox/config/plugins.py by
# compose.netbox-plugin.yaml (docs/GUI_AUTHENTICATION.md confirms
# read_configurations() loads every file in /etc/netbox/config/), so it is
# not baked into the image here. Static assets must still be collected at
# build time: NetBox's collectstatic step gathers a plugin's static files
# (e.g. admin/API browser assets touched by the plugin's app config) into
# STATIC_ROOT, and the running container does not repeat this on every
# start. SECRET_KEY is a throwaway build-time value; it is never used again
# after this RUN step and the real one comes from netbox.env at runtime.
RUN SECRET_KEY="build-time-only-not-used-at-runtime-0123456789abcdef01234567" \
    /opt/netbox/venv/bin/python /opt/netbox/netbox/manage.py collectstatic --no-input
