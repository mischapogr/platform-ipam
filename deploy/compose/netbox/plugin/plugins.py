# Mounted read-only to /etc/netbox/config/plugins.py by
# compose.netbox-plugin.yaml. docs/GUI_AUTHENTICATION.md "Verified in image
# (A1)" confirms NetBox's read_configurations() loads every file placed in
# /etc/netbox/config/, so this file is picked up automatically alongside
# configuration.py and any extra.py -- nothing else needs to reference it.
#
# Package N2 (docs/WORK_PLAN.md Track N). Enables netbox-aws-vpc-plugin only;
# see docs/NETBOX_AWS_PLUGIN.md for the evaluation and version pin, and
# deploy/compose/netbox/plugin/Dockerfile for the image that installs it.
PLUGINS = ["netbox_aws_vpc_plugin"]

# No plugin-specific configuration is needed yet. Package N3 (import into the
# plugin) may add entries here if the plugin gains configurable behaviour.
PLUGINS_CONFIG = {}
