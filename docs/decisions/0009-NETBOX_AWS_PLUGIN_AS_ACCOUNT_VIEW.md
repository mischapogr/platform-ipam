# ADR 0009: The NetBox AWS plugin is a view, never an allocation input

Status: proposed, 2026-09-18. Nothing has been installed yet; see the
[work plan](../WORK_PLAN.md), track N.

## Context

Onboarding import lands an existing network in NetBox as a prefix in the
domain's VRF carrying an import tag, a batch name and three AWS custom fields:
`platform_aws_account_id`, `platform_aws_region` and
`platform_aws_resource_id` (`internal/netbox/occupancy.go:24-40`, `161-171`).
That is enough to block address space and very little else. A VPC and its
subnets are unrelated rows with no edge between them, an account is a
twelve-digit string repeated on every prefix it owns, and everything else an
operator knows about the estate is squeezed into a description that NetBox
caps at two hundred characters.

Package N1 evaluated two third-party plugins that model these objects properly
and recommended [netbox-aws-vpc-plugin](../NETBOX_AWS_PLUGIN.md) 0.1.0. Its
three models — AWS Account, AWS VPC, AWS Subnet — are the shape the import
already produces, and its CIDR fields are ForeignKeys to `ipam.Prefix` rather
than CIDR columns of their own, so a plugin object names a prefix instead of
restating it.

What the allocator reads is far narrower than any plugin's surface. `Snapshot`
pages `/api/ipam/prefixes/`, `/api/ipam/ip-addresses/` and
`/api/ipam/ip-ranges/`, keeps what sits in the domain's VRF, and returns that
one list of occupied networks (`internal/netbox/client.go:322-444`). There is
no fourth query, and no plugin endpoint among them. A plugin object is
invisible to allocation by construction rather than by policy, which is the
whole reason this is cheap to adopt.

Accounts are configuration and only configuration. A coverage cell is rejected
at load unless the account is twelve digits and the role ARN names that same
account, and a pool whose eligible account has no coverage cell for the pool's
region fails validation outright (`internal/config/config.go:154-171`,
`192-197`). The file is read once, at start
(`cmd/platform-ipam/main.go:68`).
[ADR 0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md) settled that the
import renders YAML fragments for a person to merge rather than editing any of
that, and `docs/ONBOARDING_IMPORT.md` section 7 is where `render-config` prints
them.

[ADR 0008](0008-PLATFORM_OWNED_OPERATOR_UI.md) recommends building no
platform-owned UI, and says that if one ever becomes necessary it should be a
NetBox plugin, because a plugin inherits browser authentication that is being
paid for once already. That is a different artifact from this one. The plugin
0008 contemplates would be written by this project, would call the platform
API, and would need a mapping from NetBox users to platform principals plus a
cross-tenant read before it answered anything useful. The plugin decided here
is someone else's, calls nothing, maps nobody, and answers none of the four
questions
`docs/NETBOX_INTEGRATION.md` section 6 names. Adopting it neither implements
0008's fallback nor forecloses it.

## Decision

`netbox-aws-vpc-plugin` becomes the documented home for AWS Account, VPC and
Subnet objects in NetBox, as an optional overlay on the prefixes the import
already writes.

It is a view, never the allocator's source of truth. Every network is still a
prefix in the domain's VRF written by `EnsureOccupancy`, with the same tag,
batch and custom fields as before; a plugin object is created afterwards and
points at that prefix. A VPC that exists only as a plugin object blocks
nothing, and deleting every plugin object changes no allocation outcome. That
invariant is not a promise in prose: package N3's end-to-end test imports the
table, finds account, VPC and subnet through the plugin API, then deletes the
plugin objects and shows that reservation still skips the imported space
(`docs/WORK_PLAN.md`, package N3). A change that made the allocator consult
the plugin would have to delete that test to pass, which is the point of
writing it that way.

The plugin buys back something ADR 0007 gives up. A duplicate `(vrf, cidr)`
fails the whole inventory snapshot and returns 503 for every reservation in
the domain, so the import collapses the same CIDR appearing in two accounts
into one prefix and records the duplication as a warning
(`docs/ONBOARDING_IMPORT.md` section 5). The surviving prefix has exactly one
`platform_aws_account_id`, so the second account's claim is simply gone.
Because `AWSVPC.vpc_cidr` is a plain ForeignKey and not a one-to-one field,
two VPC objects with different `owner_account` values may point at that single
prefix, and the per-account picture the collapse destroyed comes back without
a second prefix and without touching the snapshot. The rejected alternative
plugin uses a `OneToOneField` and could not represent it at all; this is the
strongest single argument for the recommended candidate.

`onboard render-config` does not change, and the plugin does not become a way
to add an account. Coverage cells, `eligible_accounts` and identities are what
gate observation, allocation and authentication, they are validated at load,
and an AWSAccount row in NetBox grants none of them. The division is that
configuration says what is permitted and the plugin records what exists. The
obvious failure is drift in both directions: an operator creates an account in
the plugin, nobody merges the `render-config` fragment, and a request is
refused for an account the inventory plainly shows; or configuration covers an
account that has no plugin object and the operator view understates the blast
radius of a coverage change. Nothing detects either today, and no check is
proposed here; the mitigation is that `render-config` stays the only path into
configuration and the import writes both sides from the same table in the same
batch.

The overlay is optional. Package N3 puts it behind an `--aws-objects` flag on
`onboard apply`; without the flag behaviour is unchanged, so an installation
that never installs the plugin is unaffected, and neither the Helm chart nor
the API contract gains anything to describe.

The position is therefore: adopt the plugin as an operator-facing view of AWS
accounts and their networks, keep prefixes in the domain VRF as the only thing
that blocks address space, and keep configuration as the only thing that
authorizes it.

## Consequences

The upgrade coupling is real and is the largest risk accepted here. The plugin
is at version 0.1.0 with one maintainer, and its recent activity is mostly
automated dependency bumps (`docs/NETBOX_AWS_PLUGIN.md`, reviewer
verification). It declares `min_version = "4.5.0"` and no `max_version`, which
means it will load into a NetBox release it was never tested against instead
of refusing to start. NetBox 5.0 is coming and the cadence is visible in this
repository already: `LOGIN_REQUIRED` is deprecated in the pinned release and
removed in 5.0 (`docs/GUI_AUTHENTICATION.md:79`). Production must therefore
pin both the image and the plugin version and test the pair together, and an
incompatible plugin is disabled at upgrade time rather than allowed to hold
the NetBox upgrade hostage — which is what `docs/NETBOX_INTEGRATION.md`
section 6 already requires of any plugin here. Disabling it costs operators a
view and costs allocation nothing.

The exit path if the plugin is abandoned is short because the plugin owns
little. What survives in NetBox is everything that matters: the prefixes, the
`platform-ipam-imported` tag, the batch and source fields, and the AWS
account, region and resource identifiers on each prefix
(`internal/netbox/occupancy.go:24-40`). What is lost is the VPC-to-subnet
edge, the account and VPC names, ARNs and statuses, and the second owner on a
collapsed prefix. Before uninstalling, that is exported through the plugin's
own REST endpoints under `/api/plugins/aws-vpc/`; the migrations
create plugin tables only and add no column to core IPAM, so removing them
leaves the prefixes untouched. The cheaper recovery is that the import is
reproducible: the canonical table from `onboard parse` still exists, and
`onboard apply --aws-objects` is idempotent, so a replacement home can be
populated from the same input rather than from a NetBox export.

What is not known is stated plainly. At the time of writing nobody has
installed this plugin into the pinned image
(`netboxcommunity/netbox:v4.6.7-5.0.2`); package N2 is doing that in parallel,
and "supports 4.6.7" so far means declared compatible by metadata and nothing
more. Three N2 outcomes flip this record to rejected: the plugin fails to
install or its migrations fail against the pinned image; its migrations touch
core IPAM tables, which is that package's explicit stop condition and would
break the uninstall path this record relies on; or the plugin's presence makes
`run-e2e.sh` fail where it passes today. A fourth, softer outcome — the plugin
installs but requires a Python or image change to the pinned base — is not a
rejection but returns the question to N1's alternative and to doing nothing.
