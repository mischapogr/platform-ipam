from netbox.plugins import PluginConfig


class PlatformIpamWorkspaceConfig(PluginConfig):
    name = "platform_ipam_workspace"
    verbose_name = "Platform IPAM migration workspace"
    description = "Read-only overlap, migration and topology evidence in NetBox"
    version = "0.1.0"
    base_url = "platform-ipam"
    min_version = "4.7.1"
    max_version = "4.7.1"


config = PlatformIpamWorkspaceConfig
