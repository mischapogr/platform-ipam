from netbox.plugins import PluginMenuItem


menu_items = (
    PluginMenuItem(
        link="plugins:platform_ipam_workspace:workspace",
        link_text="Migration workspace",
        auth_required=True,
    ),
)
