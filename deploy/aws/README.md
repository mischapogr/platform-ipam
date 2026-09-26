# `deploy/aws` — cross-account read-only role

`platform-ipam-readonly-role.yaml` is a CloudFormation template for the IAM
role `PlatformIpamReadOnly` (name configurable). It is the **only** AWS write
this repository proposes making into a member account, and it grants no
write permission at all: it is a read-only observation role.

## What the role is for

Two different callers assume it, at different times, for the same three
region-scoped EC2 Describe actions:

- **The platform-ipam worker**, continuously, to observe VPC/subnet/AZ
  inventory in each account and region it reconciles against
  (`docs/AWS_INTEGRATION.md` section 5). The worker's base role
  (`PlatformIpamWorkerProd` or similar) is `TrustedPrincipalArn`, and the
  `EksClusterArn` / `KubernetesNamespace` / `KubernetesServiceAccount`
  parameters should be set to require the worker's Pod Identity session
  tags, matching the target-role trust example in that section.
- **The one-off organization inventory operator**
  (`docs/AWS_ORGANIZATION_INVENTORY.md` section 1), who additionally needs
  `ec2:DescribeRegions` to discover which regions to scan. Set
  `EnableRegionDiscovery=true` and `TrustedPrincipalArn` to the operator's
  own role for a role deployed to support that one-off task; leave the three
  Kubernetes tag parameters empty since there is no EKS workload identity to
  match.

The topology collector is a third, opt-in use of this template. Deploy it as
`PlatformIpamTopologyReadOnly` with `EnableTopologyDiscovery=true`,
`TrustedPrincipalArn` set to the operator role, and the reviewed
`AllowedRegions`. Its extra actions are `DescribeRouteTables`,
`DescribeTransitGateways`, `DescribeTransitGatewayAttachments`,
`DescribeTransitGatewayVpcAttachments`, `DescribeTransitGatewayRouteTables`,
`GetTransitGatewayRouteTableAssociations`,
`GetTransitGatewayRouteTablePropagations`, and `SearchTransitGatewayRoutes`.
They are restricted by `ec2:Region`. Keep the flag false for worker roles.
See [topology inventory](../../docs/AWS_TOPOLOGY_INVENTORY.md).

These are separate deployments of the same template with different
parameter values. A single StackSet can only carry one set
of parameters at a time, so if both use cases are needed simultaneously
across the same accounts, deploy the template twice under two StackSet
names (e.g. `platform-ipam-readonly-role` for the worker,
`platform-ipam-readonly-role-inventory` for the one-off operator), or narrow
`EnableRegionDiscovery=true` to a short-lived StackSet you delete once the
inventory procedure is done.
Use a third StackSet for topology reads.

The base role grants exactly these inventory actions, scoped by an
`ec2:Region` condition to `AllowedRegions`, `Resource: "*"`:

- `ec2:DescribeVpcs`
- `ec2:DescribeSubnets`
- `ec2:DescribeAvailabilityZones`
- `ec2:DescribeRegions` — only when `EnableRegionDiscovery=true`

The eight topology actions above are added only when
`EnableTopologyDiscovery=true`. `DescribeRegions` is independent of that flag.

No `Create*`, `Delete*`, `Modify*`, `Associate*`, broad `ec2:Describe*`, or
`organizations:*` permission is ever included, and the trust policy's
principal is always a single parameterized role ARN — never a wildcard.

## Deploying as a service-managed StackSet

This targets the whole AWS Organization (or an OU) from the **management
account**, or a registered delegated administrator account, using AWS
Organizations integration so CloudFormation manages the per-account IAM
roles for you. Prerequisite: trusted access between CloudFormation StackSets
and AWS Organizations must already be enabled for the organization.

```bash
aws cloudformation create-stack-set \
  --stack-set-name platform-ipam-readonly-role \
  --template-body file://deploy/aws/platform-ipam-readonly-role.yaml \
  --permission-model SERVICE_MANAGED \
  --auto-deployment Enabled=true,RetainStacksOnAccountRemoval=false \
  --parameters \
      ParameterKey=RoleName,ParameterValue=PlatformIpamReadOnly \
      ParameterKey=AllowedRegions,ParameterValue="eu-central-1\,eu-west-1" \
      ParameterKey=TrustedPrincipalArn,ParameterValue=arn:aws:iam::111122223333:role/PlatformIpamWorkerProd \
      ParameterKey=EksClusterArn,ParameterValue=arn:aws:eks:eu-central-1:111122223333:cluster/platform-prod \
      ParameterKey=KubernetesNamespace,ParameterValue=platform-ipam-prod \
      ParameterKey=KubernetesServiceAccount,ParameterValue=platform-ipam-worker \
      ParameterKey=EnableRegionDiscovery,ParameterValue=false

aws cloudformation create-stack-instances \
  --stack-set-name platform-ipam-readonly-role \
  --deployment-targets OrganizationalUnitIds=<root-or-ou-id> \
  --regions <home-region-for-the-stack-set-operation>
```

`--deployment-targets OrganizationalUnitIds` accepts either the
organization's root ID (`r-xxxx`, deploys to every account in the
organization) or a specific OU ID (deploys to that OU and its children).
`--regions` here is the CloudFormation operation's home region for creating
the IAM role stack in each target account — it is unrelated to
`AllowedRegions`, which is an IAM policy condition on which EC2 regions the
resulting role may read.

For the one-off inventory role, repeat both commands with a different
`--stack-set-name`, `TrustedPrincipalArn` set to the inventory operator's
role, `EnableRegionDiscovery=true`, and the three Kubernetes tag parameters
left empty (their default).

**Service-managed StackSets never deploy to the management account itself**,
even when the management account is included in the targeted OU — this is
documented AWS behavior, not a limitation of this template. If the
management account also needs the role (for example, to run the inventory
procedure from that account), create it there with a plain
`aws cloudformation create-stack` (or manually), not through this StackSet.

## Removing or updating

`update-stack-set` changes the template or parameters for every deployed
instance. Deleting stack instances first (`delete-stack-instances`) before
deleting the StackSet itself follows the same lifecycle as any other
CloudFormation StackSet.

## What was not verified

Nothing in this directory was deployed or validated against a live AWS
account or a live AWS Organization. `cfn-lint` was run against the template
in a container (see `docs/WORK_PLAN.md` package B3), and a local script
confirmed the rendered action list matches exactly the actions listed above.
Effective account permissions, service control policies, StackSet trusted
access, and the correctness of any example ARN or OU ID above have not been
checked against a real organization.
