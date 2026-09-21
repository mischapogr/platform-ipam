package adoptcmd

import (
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func testRecord() Record {
	return Record{
		SourceRow: 2, TenantID: "team-a", AllocationKey: "vpc-orders", Scope: "vpc",
		Environment: "prod", Region: "eu-central-1", AccountID: "123456789012",
		CIDR: "10.1.0.0/24", ResourceID: "vpc-0abc", NetBoxPrefixID: "42",
	}
}

func TestResolvePrincipalExactlyOneCoveringIdentity(t *testing.T) {
	identities := []domain.Principal{
		{Subject: "team-a-prod", TenantID: "team-a", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "team-a-stage", TenantID: "team-a", Accounts: []string{"999999999999"}, Environments: []string{"stage"}, Regions: []string{"eu-central-1"}},
		{Subject: "team-b-prod", TenantID: "team-b", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
	}
	p, err := resolvePrincipal(identities, testRecord())
	if err != nil {
		t.Fatalf("resolvePrincipal: %v", err)
	}
	if p.Subject != "team-a-prod" {
		t.Fatalf("want team-a-prod, got %#v", p)
	}
}

func TestResolvePrincipalNoIdentityForTenant(t *testing.T) {
	_, err := resolvePrincipal(nil, testRecord())
	if err == nil || !strings.Contains(err.Error(), "has no identity") {
		t.Fatalf("want a no-identity error, got %v", err)
	}
}

func TestResolvePrincipalNoIdentityCoversScope(t *testing.T) {
	identities := []domain.Principal{
		{Subject: "team-a-stage", TenantID: "team-a", Accounts: []string{"123456789012"}, Environments: []string{"stage"}, Regions: []string{"eu-central-1"}},
	}
	_, err := resolvePrincipal(identities, testRecord())
	if err == nil || !strings.Contains(err.Error(), "no identity") || !strings.Contains(err.Error(), "covers") {
		t.Fatalf("want a scope-not-covered error, got %v", err)
	}
}

func TestResolvePrincipalRefusesAmbiguousCoverageRatherThanChoosing(t *testing.T) {
	identities := []domain.Principal{
		{Subject: "team-a-1", TenantID: "team-a", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "team-a-2", TenantID: "team-a", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
	}
	_, err := resolvePrincipal(identities, testRecord())
	if err == nil || !strings.Contains(err.Error(), "2 identities") {
		t.Fatalf("want an ambiguous-identity error, got %v", err)
	}
}

// An identity without a tenant is what ADR 0011's operator is. A record that
// names no tenant must never be matched to it: the acting principal is always
// a tenant's.
func TestResolvePrincipalNeverMatchesAnIdentityWithoutATenant(t *testing.T) {
	identities := []domain.Principal{{Subject: "ops-operator"}}
	r := testRecord()
	r.TenantID = ""
	if p, err := resolvePrincipal(identities, r); err == nil {
		t.Fatalf("a record without a tenant resolved to %#v", p)
	}
}
