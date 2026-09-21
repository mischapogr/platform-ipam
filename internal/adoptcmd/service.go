// Package adoptcmd implements the `platform-ipam adopt plan|apply` process
// mode (docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md,
// docs/WORK_PLAN.md package F4): the operator tool that turns a reviewed
// table of existing networks into owned allocations. It is a sibling of
// `onboard`, not a subcommand of it: unlike onboard, adopt opens the ledger
// and needs the cloud observer, so it dispatches where the api and worker
// modes do in cmd/platform-ipam/main.go, after settings and configuration are
// validated, never before.
//
// This package holds no allocation policy of its own. Every rule that decides
// whether a record is adoptable lives in internal/service (validateRequest,
// pinnedCIDR, and the read-only PlanAdoption this package calls for `plan`);
// adoptcmd only parses the reviewed table, resolves the acting principal from
// configuration, orders parents before subnets, and reports what the service
// decided. Depending on the small Service interface below, rather than
// *service.Service directly, is what lets its tests use a fake service with
// no ledger, NetBox or cloud observer at all.
//
// # Input format
//
// One reviewed record per network, as a CSV or TSV table with a header row.
// Every column below is required on every row; an unrecognized column, a
// missing required column, or a column supplying a value this package
// derives itself (prefix_length, address_family, pool, domain, ...) is an
// error -- unlike onboard's tables, nothing here is normalized, repaired, or
// silently kept as a description. TSV is selected by a ".tsv" or ".txt"
// extension; everything else is read as comma-separated. ".xlsx" is not
// supported: internal/onboard's xlsx reader is closed over its own three
// table kinds through unexported functions this package cannot call without
// either duplicating that reader or editing internal/onboard, which this
// package's work order forbids -- export to CSV instead.
//
//	tenant_id               owning tenant, as configured in the identity file
//	allocation_key          exactly as the owning team will use it in their own POST
//	scope                   "vpc" or "subnet"
//	environment             matched against the tenant's identity and the pool
//	region                  matched against the tenant's identity and the pool
//	account_id              matched against the tenant's identity and the pool;
//	                        compared with the observed resource by the service,
//	                        never trusted on its own
//	cidr                    the exact, canonical IPv4 CIDR reviewed; its mask
//	                        supplies prefix_length, which may not be a column
//	resource_id             the AWS resource id observed at that CIDR
//	netbox_prefix_id        the NetBox id of the imported, unmanaged prefix at
//	                        that CIDR
//	parent_allocation_key   required for scope=subnet, forbidden for scope=vpc:
//	                        the allocation_key of the parent vpc record, either
//	                        already committed in the ledger or present as its
//	                        own vpc row elsewhere in this same file
//	availability_zone_id    required for scope=subnet, forbidden for scope=vpc
//
// A subnet's parent is named by allocation key, never by allocation id: the
// parent's id does not exist until the parent itself is adopted, possibly in
// this same run. `apply` resolves it from the ledger result of an
// already-adopted parent, or from this run's own adoption of a parent row
// earlier in the file (ordering.go decides that order); `plan` reports such a
// subnet's verdict as "deferred" rather than refusing it, since its real
// admissibility depends on an outcome `plan` never produces.
package adoptcmd

import (
	"context"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

// Service is the minimal surface adoptcmd needs from *service.Service:
// PlanAdoption and Adopt for one record, List to resolve a parent's
// allocation id from an already-committed adoption, and -- package H2c --
// AbandonAdoption and PlanAbandonAdoption for `adopt abandon`. main.go passes
// the real *service.Service, which already satisfies this interface with no
// changes; tests pass a fake with no ledger, NetBox or cloud observer behind
// it.
type Service interface {
	PlanAdoption(ctx context.Context, p domain.Principal, req domain.Request, pin service.Adoption) (*service.AdoptionVerdict, error)
	Adopt(ctx context.Context, p domain.Principal, req domain.Request, pin service.Adoption) (*domain.Allocation, *domain.Operation, int, error)
	List(ctx context.Context, p domain.Principal) ([]domain.Allocation, error)
	// AbandonAdoption and PlanAbandonAdoption take no domain.Principal, exactly
	// as *service.Service's own methods do not: an abandon acts on the
	// ledger's hold, not as a tenant (ADR 0012).
	AbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*service.AbandonReport, error)
	PlanAbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*service.AbandonReport, error)
}
