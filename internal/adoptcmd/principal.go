package adoptcmd

// The acting principal is built from configuration, never from the reviewed
// table: adopt acts AS the tenant without that tenant's authentication (ADR
// 0010), so which identity it acts as is a deployment decision, not something
// a CSV column may assert. --operator names the person running the process
// for the audit trail (service.Adoption.Operator) and is never looked up here
// or granted anything.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// resolvePrincipal picks the configured identity that acts for one record.
// The rule: among the tenant's identities, exactly one must have a scope
// (accounts, environments, regions) that covers this record's account,
// environment and region; that one becomes the principal, unchanged. Zero
// covering identities is refused rather than assumed; more than one is also
// refused rather than picking arbitrarily, because "several identities with
// different scopes" (docs/WORK_PLAN.md package F4) means the configuration
// itself has not said which one owns this exact target, and adopt never
// widens a principal's scope to make a record fit.
func resolvePrincipal(identities []domain.Principal, r Record) (domain.Principal, error) {
	// A record without a tenant must never match an identity without one: the
	// acting principal is a tenant's, and ADR 0011's operator is exactly an
	// identity that has none.
	if r.TenantID == "" {
		return domain.Principal{}, fmt.Errorf("the record names no tenant")
	}
	var tenantIdentities []domain.Principal
	for _, id := range identities {
		if id.TenantID != "" && id.TenantID == r.TenantID {
			tenantIdentities = append(tenantIdentities, id)
		}
	}
	if len(tenantIdentities) == 0 {
		return domain.Principal{}, fmt.Errorf("tenant %q has no identity in configuration", r.TenantID)
	}

	var covering []domain.Principal
	for _, id := range tenantIdentities {
		if scopeCovers(id, r.AccountID, r.Environment, r.Region) {
			covering = append(covering, id)
		}
	}
	switch len(covering) {
	case 0:
		return domain.Principal{}, fmt.Errorf(
			"no identity for tenant %q covers account %s, environment %s, region %s; refusing rather than widening scope",
			r.TenantID, r.AccountID, r.Environment, r.Region)
	case 1:
		return covering[0], nil
	default:
		subjects := make([]string, len(covering))
		for i, id := range covering {
			subjects[i] = id.Subject
		}
		sort.Strings(subjects)
		return domain.Principal{}, fmt.Errorf(
			"tenant %q has %d identities whose scope covers account %s, environment %s, region %s (%s); refusing rather than choosing one",
			r.TenantID, len(covering), r.AccountID, r.Environment, r.Region, strings.Join(subjects, ", "))
	}
}

// scopeCovers reports whether id's scope covers a target, using the same
// "empty means unrestricted" rule internal/service's own eligibility checks
// use (internal/config.Validate never actually leaves an identity's lists
// empty, so this only matters if that ever changes).
func scopeCovers(id domain.Principal, accountID, environment, region string) bool {
	return containsOrEmpty(id.Accounts, accountID) &&
		containsOrEmpty(id.Environments, environment) &&
		containsOrEmpty(id.Regions, region)
}

func containsOrEmpty(xs []string, v string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
