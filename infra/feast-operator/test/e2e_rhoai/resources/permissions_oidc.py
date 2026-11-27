from feast.feast_object import ALL_RESOURCE_TYPES
from feast.permissions.permission import Permission
from feast.permissions.action import ALL_ACTIONS
from feast.permissions.policy import GroupBasedPolicy

# Define admin groups with full access
admin_groups = ["dedicated-admins"]

# Admin group permission — full control over all Feast resources
admin_group_perm = Permission(
    name="admin_group_permission",
    types=ALL_RESOURCE_TYPES,
    policy=GroupBasedPolicy(admin_groups),
    actions=ALL_ACTIONS,
)

print("Admin group permission configured successfully.")
