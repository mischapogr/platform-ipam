package adoptcmd

// fakeService is the small fake this package's own tests use in place of
// *service.Service, so adoptcmd's ordering, flag parsing, exit codes and
// reporting can be tested with no ledger, NetBox or cloud observer at all
// (docs/WORK_PLAN.md package F4). It implements the Service interface
// directly with canned, per-call responses keyed by tenant and allocation
// key: internal/service's own safety rules (pinnedCIDR, validateRequest,
// the replay/retirement logic) are internal/service's tests to hold, not
// this package's to re-derive.

import (
	"context"
	"fmt"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

type fakeKey struct{ tenant, key string }

// fakePlanResult is one canned PlanAdoption response: either verdict or err
// is set, matching PlanAdoption's own (result, error) shape.
type fakePlanResult struct {
	verdict *service.AdoptionVerdict
	err     error
}

// fakeAdoptResult is one canned Adopt response.
type fakeAdoptResult struct {
	allocation *domain.Allocation
	status     int
	err        error
}

// fakeAbandonResult is one canned AbandonAdoption/PlanAbandonAdoption
// response, keyed by allocation id rather than by fakeKey: abandon takes no
// domain.Principal and no allocation_key, only the allocation id an apply
// report or a finding already produced.
type fakeAbandonResult struct {
	report *service.AbandonReport
	err    error
}

type fakeService struct {
	plan    map[fakeKey]fakePlanResult
	adopt   map[fakeKey]fakeAdoptResult
	list    map[string][]domain.Allocation
	listErr error

	abandon     map[string]fakeAbandonResult
	planAbandon map[string]fakeAbandonResult

	planCalls        []planCall
	adoptCalls       []adoptCall
	abandonCalls     []abandonCall
	planAbandonCalls []abandonCall
}

type planCall struct {
	principal domain.Principal
	request   domain.Request
	pin       service.Adoption
}
type adoptCall struct {
	principal domain.Principal
	request   domain.Request
	pin       service.Adoption
}

// abandonCall records every argument AbandonAdoption/PlanAbandonAdoption were
// given, verbatim, so a test can assert the command line reached the service
// unchanged.
type abandonCall struct {
	allocationID, operationID, operator, reason string
}

func newFakeService() *fakeService {
	return &fakeService{
		plan:        map[fakeKey]fakePlanResult{},
		adopt:       map[fakeKey]fakeAdoptResult{},
		list:        map[string][]domain.Allocation{},
		abandon:     map[string]fakeAbandonResult{},
		planAbandon: map[string]fakeAbandonResult{},
	}
}

func (f *fakeService) setPlan(tenant, key string, v *service.AdoptionVerdict, err error) {
	f.plan[fakeKey{tenant, key}] = fakePlanResult{verdict: v, err: err}
}
func (f *fakeService) setAdopt(tenant, key string, a *domain.Allocation, status int, err error) {
	f.adopt[fakeKey{tenant, key}] = fakeAdoptResult{allocation: a, status: status, err: err}
}

func (f *fakeService) PlanAdoption(ctx context.Context, p domain.Principal, req domain.Request, pin service.Adoption) (*service.AdoptionVerdict, error) {
	f.planCalls = append(f.planCalls, planCall{p, req, pin})
	r, ok := f.plan[fakeKey{p.TenantID, req.AllocationKey}]
	if !ok {
		return nil, fmt.Errorf("fakeService: no PlanAdoption response configured for tenant %q key %q", p.TenantID, req.AllocationKey)
	}
	return r.verdict, r.err
}

func (f *fakeService) Adopt(ctx context.Context, p domain.Principal, req domain.Request, pin service.Adoption) (*domain.Allocation, *domain.Operation, int, error) {
	f.adoptCalls = append(f.adoptCalls, adoptCall{p, req, pin})
	r, ok := f.adopt[fakeKey{p.TenantID, req.AllocationKey}]
	if !ok {
		return nil, nil, 0, fmt.Errorf("fakeService: no Adopt response configured for tenant %q key %q", p.TenantID, req.AllocationKey)
	}
	return r.allocation, nil, r.status, r.err
}

func (f *fakeService) List(ctx context.Context, p domain.Principal) ([]domain.Allocation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list[p.TenantID], nil
}

func (f *fakeService) setAbandon(allocationID string, r *service.AbandonReport, err error) {
	f.abandon[allocationID] = fakeAbandonResult{report: r, err: err}
}
func (f *fakeService) setPlanAbandon(allocationID string, r *service.AbandonReport, err error) {
	f.planAbandon[allocationID] = fakeAbandonResult{report: r, err: err}
}

func (f *fakeService) AbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*service.AbandonReport, error) {
	f.abandonCalls = append(f.abandonCalls, abandonCall{allocationID, operationID, operator, reason})
	r, ok := f.abandon[allocationID]
	if !ok {
		return nil, fmt.Errorf("fakeService: no AbandonAdoption response configured for allocation %q", allocationID)
	}
	return r.report, r.err
}

func (f *fakeService) PlanAbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*service.AbandonReport, error) {
	f.planAbandonCalls = append(f.planAbandonCalls, abandonCall{allocationID, operationID, operator, reason})
	r, ok := f.planAbandon[allocationID]
	if !ok {
		return nil, fmt.Errorf("fakeService: no PlanAbandonAdoption response configured for allocation %q", allocationID)
	}
	return r.report, r.err
}
