package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func TestPolicySafety(t *testing.T) {
	cfg, err := Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg, "prod"); err != nil {
		t.Fatal(err)
	}
	cfg.Domains[0].CloudCoverage = nil
	if Validate(cfg, "development") == nil {
		t.Fatal("missing coverage accepted")
	}
	cfg, _ = Load("../../examples/config/pools.yaml", "", "development")
	cfg.Pools[0].ExcludedCIDRs = []string{"192.168.0.0/24"}
	if Validate(cfg, "development") == nil {
		t.Fatal("outside exclusion accepted")
	}
	cfg, _ = Load("../../examples/config/pools.yaml", "", "development")
	cfg.Lifecycle.QuarantineHours = 0
	if Validate(cfg, "prod") == nil {
		t.Fatal("production bypass accepted")
	}
}

func TestSettingsFailClosed(t *testing.T) {
	s := Settings{Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full", AuthMode: "local", AWSMode: "fake"}
	if s.Validate("api") == nil {
		t.Fatal("unsafe production accepted")
	}
	if secureOrigin("https://token@example.org", false) == nil {
		t.Fatal("URL credential accepted")
	}
}

// TestLocalExtraCredentialsRequireDevelopment is the settings-validation half
// of package G3c's "two guards on purpose" for IPAM_LOCAL_EXTRA_CREDENTIALS;
// internal/transport's NewAuth carries the independent second guard. The
// stage baseline here is otherwise entirely valid (oidc auth, live AWS,
// HTTPS NetBox) precisely so that adding IPAM_LOCAL_EXTRA_CREDENTIALS is the
// only thing that can make it fail -- proving this specific rule fires,
// rather than piggy-backing on the unrelated "stage/prod require OIDC" guard
// that would also reject local auth mode outright.
func TestLocalExtraCredentialsRequireDevelopment(t *testing.T) {
	stageBaseline := Settings{
		Environment: "stage", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
		AuthMode: "oidc", OIDCIssuer: "https://idp.example.org", OIDCAudience: "platform-ipam-stage",
		AWSMode: "live", NetBoxURL: "https://netbox.stage.example.org", NetBoxToken: "netbox-token",
	}
	if err := stageBaseline.Validate("api"); err != nil {
		t.Fatalf("expected the stage baseline to be valid on its own, got %v", err)
	}
	withExtra := stageBaseline
	withExtra.LocalExtraCredentials = "ops-observer:ops-development-token-1234567890"
	if err := withExtra.Validate("api"); err == nil {
		t.Fatal("IPAM_LOCAL_EXTRA_CREDENTIALS accepted outside development")
	}

	dev := Settings{
		Environment: "development", DatabaseURL: "postgres://localhost/ipam",
		AuthMode: "local", LocalToken: "local-development-token-123456", LocalSubject: "developer",
		LocalExtraCredentials: "ops-observer:ops-development-token-1234567890",
		AWSMode:               "fake", FakeCloudFile: "fixtures/cloud.json",
		NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
	}
	if err := dev.Validate("api"); err != nil {
		t.Fatalf("IPAM_LOCAL_EXTRA_CREDENTIALS refused in development: %v", err)
	}
}

func TestMigrationRequiresVerifiedDatabaseTLS(t *testing.T) {
	s := Settings{Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=disable"}
	if s.Validate("migrate") == nil {
		t.Fatal("production migration accepted unverified TLS")
	}
	s.DatabaseURL = "postgres://localhost/ipam?sslmode=verify-full"
	if err := s.Validate("migrate"); err != nil {
		t.Fatal(err)
	}
}

// TestEmptyEligibleTenantsEntryFailsLoad is the config-validation half of
// ADR 0011 stage one: an empty entry in a pool's eligible_tenants would
// store findings with an empty tenant (internal/service/worker.go's fan-out
// takes the tenant straight from this list) and would make eligibleString
// match a tenant-less principal. Whitespace-only is treated as empty too --
// there is no legitimate tenant ID made only of whitespace.
func TestEmptyEligibleTenantsEntryFailsLoad(t *testing.T) {
	for _, tenants := range [][]string{
		{"orders-team", ""},
		{"orders-team", "   "},
		{"orders-team", "\t\n"},
	} {
		cfg, err := Load("../../examples/config/pools.yaml", "", "development")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Pools[0].EligibleTenants = tenants
		if Validate(cfg, "development") == nil {
			t.Fatalf("eligible_tenants %q accepted", tenants)
		}
	}
	// A genuinely non-empty entry must still load: this rule is additive,
	// not a tightening of what already worked.
	cfg, err := Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pools[0].EligibleTenants = []string{"orders-team", "another-team"}
	if err := Validate(cfg, "development"); err != nil {
		t.Fatalf("legitimate multi-tenant eligible_tenants rejected: %v", err)
	}
}

// TestIdentitiesWithoutPool covers zero, one and several identities whose
// tenant appears in no pool's eligible_tenants, and proves an identity whose
// tenant IS eligible for a pool is never returned. Order must be
// deterministic and match declaration order in cfg.Identities, since
// cmd/platform-ipam/main.go logs one warning per entry returned.
func TestIdentitiesWithoutPool(t *testing.T) {
	cfg, err := Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	// pools[0].eligible_tenants is [orders-team] in examples/config/pools.yaml.

	// Zero: every identity's tenant is eligible for a pool.
	cfg.Identities = []domain.Principal{{Subject: "a", TenantID: "orders-team"}}
	if got := IdentitiesWithoutPool(cfg); len(got) != 0 {
		t.Fatalf("eligible tenant reported as pool-less: %v", got)
	}

	// One: a second identity under a tenant no pool names.
	cfg.Identities = []domain.Principal{
		{Subject: "a", TenantID: "orders-team"},
		{Subject: "b", TenantID: "ops-team"},
	}
	if got := IdentitiesWithoutPool(cfg); len(got) != 1 || got[0].Subject != "b" || got[0].TenantID != "ops-team" {
		t.Fatalf("expected exactly identity b, got %v", got)
	}

	// Several, in declaration order, eligible identity excluded regardless
	// of its position.
	cfg.Identities = []domain.Principal{
		{Subject: "b", TenantID: "ops-team"},
		{Subject: "c", TenantID: "finance-team"},
		{Subject: "a", TenantID: "orders-team"},
		{Subject: "d", TenantID: "finance-team"},
	}
	got := IdentitiesWithoutPool(cfg)
	if len(got) != 3 || got[0].Subject != "b" || got[1].Subject != "c" || got[2].Subject != "d" {
		t.Fatalf("expected [b, c, d] in declaration order, got %v", got)
	}
}

// TestShippedConfigsLoad guards against the new eligible_tenants rule (or
// any future Validate change) silently breaking a config this repository
// actually ships. deploy/environments/*/values.yaml is a Helm values file
// whose policy.data is filled in at deploy time (empty in the repository),
// not a platform-ipam config document, so it is not a Load target here.
func TestShippedConfigsLoad(t *testing.T) {
	for _, path := range []string{
		"../../examples/config/pools.yaml",
		"../../deploy/compose/fixtures/pools.yaml",
	} {
		if _, err := Load(path, "", "development"); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

// -- package G3b1: the operator role -----------------------------------------
//
// ADR 0011 stage two: an operator is a principal with NO tenant. This
// package's half of the contract is the identity file's shape -- Validate's
// rules and IdentitiesWithoutPool's exclusion -- not the deny-by-default
// proof at each endpoint, which belongs to internal/transport (see
// http_test.go's noPoolBoundary-style operator boundary and
// TestTenantAndInternalFields' sibling).

// withIdentities loads the shipped example pools/domains (otherwise valid)
// with cfg.Identities replaced, for tests that only care about identity
// validation and want a realistic non-identity baseline underneath it.
func withIdentities(t *testing.T, identities []domain.Principal) domain.Config {
	t.Helper()
	cfg, err := Load("../../examples/config/pools.yaml", "", "development")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Identities = identities
	return cfg
}

// TestIdentityRoleMustBeEmptyOrOperator is ADR 0011 stage two's first rule: a
// role that is neither empty nor "operator" fails the load rather than
// quietly meaning "no role" -- a typo in case, plurality, stray whitespace,
// or an unrelated word must not silently become a privilege, nor silently
// become none.
func TestIdentityRoleMustBeEmptyOrOperator(t *testing.T) {
	for _, role := range []string{"Operator", "operators", " operator", "admin"} {
		t.Run(strconv.Quote(role), func(t *testing.T) {
			// On a tenant-shaped identity and on an operator-shaped one: the
			// second is the case that matters, because a near-miss that were
			// taken for the operator role would be refused on the first shape
			// for carrying a tenant, and pass this test for the wrong reason.
			tenantShaped := domain.Principal{
				Subject: "bad-role", TenantID: "orders-team",
				Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"},
				Role: role,
			}
			operatorShaped := domain.Principal{Subject: "bad-role", Role: role}
			for shape, identity := range map[string]domain.Principal{"tenant-shaped": tenantShaped, "operator-shaped": operatorShaped} {
				if identity.IsOperator() {
					t.Fatalf("role %q counts as the operator role", role)
				}
				if Validate(withIdentities(t, []domain.Principal{identity}), "development") == nil {
					t.Fatalf("role %q accepted on a %s identity", role, shape)
				}
			}
		})
	}
	// The two legal values still load: empty (an ordinary tenant identity)...
	cfg := withIdentities(t, []domain.Principal{{
		Subject: "ordinary", TenantID: "orders-team",
		Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"},
	}})
	if err := Validate(cfg, "development"); err != nil {
		t.Fatalf("empty role rejected: %v", err)
	}
	// ...and exactly "operator", with none of the four tenant-shaped fields.
	cfg = withIdentities(t, []domain.Principal{{Subject: "ops-operator", Role: domain.OperatorRole}})
	if err := Validate(cfg, "development"); err != nil {
		t.Fatalf("operator role rejected: %v", err)
	}
}

// TestOperatorIdentityRequiresSubject proves the operator branch does not
// bypass the subject requirement every identity already has: an operator
// entry with no subject fails the load exactly as a non-operator one would.
func TestOperatorIdentityRequiresSubject(t *testing.T) {
	cfg := withIdentities(t, []domain.Principal{{Role: domain.OperatorRole}})
	if Validate(cfg, "development") == nil {
		t.Fatal("operator identity with no subject accepted")
	}
}

// TestOperatorIdentityCarriesNoTenantOrScope is ADR 0011 stage two's second
// rule, tested one field at a time: an operator carrying tenant_id would make
// it a tenant with cross-tenant reads and conflate the audit trail; carrying
// accounts, environments or regions would sit on the principal looking like a
// constraint while constraining nothing, because only validateRequest and
// capacity ever consult them and neither is reachable without a tenant.
func TestOperatorIdentityCarriesNoTenantOrScope(t *testing.T) {
	cases := []struct {
		name string
		id   domain.Principal
	}{
		{"tenant_id", domain.Principal{Subject: "ops-operator", Role: domain.OperatorRole, TenantID: "ops"}},
		{"accounts", domain.Principal{Subject: "ops-operator", Role: domain.OperatorRole, Accounts: []string{"123456789012"}}},
		{"environments", domain.Principal{Subject: "ops-operator", Role: domain.OperatorRole, Environments: []string{"prod"}}},
		{"regions", domain.Principal{Subject: "ops-operator", Role: domain.OperatorRole, Regions: []string{"eu-central-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := withIdentities(t, []domain.Principal{tc.id})
			if Validate(cfg, "development") == nil {
				t.Fatalf("operator carrying %s accepted", tc.name)
			}
		})
	}
	// The clean operator -- none of the four -- loads.
	cfg := withIdentities(t, []domain.Principal{{Subject: "ops-operator", Role: domain.OperatorRole}})
	if err := Validate(cfg, "development"); err != nil {
		t.Fatalf("clean operator identity rejected: %v", err)
	}
}

// TestNonOperatorIdentityRulesUnchanged proves G3b1 left every existing
// identity rule for an ordinary (non-operator) principal exactly as it was.
// The pre-G3b1 code checked these conditions in one combined guard; they are
// now split across two (subject/duplicate first, then tenant scoping), and
// must still refuse or accept exactly as before, byte-for-byte in effect.
func TestNonOperatorIdentityRulesUnchanged(t *testing.T) {
	valid := domain.Principal{Subject: "ordinary", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}}
	cases := []struct {
		name string
		id   domain.Principal
		ok   bool
	}{
		{"valid identity still loads", valid, true},
		{"missing subject", func() domain.Principal { p := valid; p.Subject = ""; return p }(), false},
		{"missing tenant_id", func() domain.Principal { p := valid; p.TenantID = ""; return p }(), false},
		{"missing accounts", func() domain.Principal { p := valid; p.Accounts = nil; return p }(), false},
		{"missing environments", func() domain.Principal { p := valid; p.Environments = nil; return p }(), false},
		{"missing regions", func() domain.Principal { p := valid; p.Regions = nil; return p }(), false},
		{"invalid account pattern", func() domain.Principal { p := valid; p.Accounts = []string{"not-an-account"}; return p }(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := withIdentities(t, []domain.Principal{tc.id})
			err := Validate(cfg, "development")
			if tc.ok && err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// TestDuplicateSubjectAcrossOperatorAndTenant proves the existing
// duplicate-subject rule applies across both kinds: one subject can never be
// both a tenant and an operator.
func TestDuplicateSubjectAcrossOperatorAndTenant(t *testing.T) {
	cfg := withIdentities(t, []domain.Principal{
		{Subject: "shared", TenantID: "orders-team", Accounts: []string{"123456789012"}, Environments: []string{"prod"}, Regions: []string{"eu-central-1"}},
		{Subject: "shared", Role: domain.OperatorRole},
	})
	if Validate(cfg, "development") == nil {
		t.Fatal("one subject accepted as both a tenant and an operator")
	}
}

// TestIdentitiesWithoutPoolSkipsOperators proves an operator, which has no
// tenant by construction, is never reported by IdentitiesWithoutPool: the
// warning it feeds ("this identity's tenant is eligible for no pool") would
// be false for a principal that has no tenant to be ineligible with.
func TestIdentitiesWithoutPoolSkipsOperators(t *testing.T) {
	cfg := withIdentities(t, []domain.Principal{
		{Subject: "a", TenantID: "orders-team"},
		{Subject: "ops-operator", Role: domain.OperatorRole},
		{Subject: "b", TenantID: "ops-team"},
	})
	got := IdentitiesWithoutPool(cfg)
	if len(got) != 1 || got[0].Subject != "b" {
		t.Fatalf("expected only identity b, got %v", got)
	}
}

// TestIdentityFileUnknownFieldFailsLoad records the rollback fact: strict
// KnownFields decoding is what makes an identity file carrying an
// unrecognized key fail Load outright, rather than the field being silently
// ignored. This is also why adding a role: key to a shipped identity file is
// a forward-incompatible change (ADR 0011): a binary built before
// Principal.Role sees "role" as exactly this kind of unknown field and
// refuses to start, loudly, rather than starting with the field silently
// dropped. So the deployment order is: the binary first, then the file that
// uses role; a rollback needs the old file back before the old binary.
func TestIdentityFileUnknownFieldFailsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identities.yaml")
	body := `identities:
  - subject: someone
    tenant_id: orders-team
    accounts: ["123456789012"]
    environments: [prod]
    regions: [eu-central-1]
    unknown_field: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("../../examples/config/pools.yaml", path, "development"); err == nil {
		t.Fatal("identity file with an unknown field was accepted")
	}
}

// TestOperatorRoleOnlyConstructedByConfigLoad is package G3b1's sibling to
// F2's "three places" source-parsing test: nothing outside this package's
// Load (whose YAML decode sets struct fields by reflection, never by writing
// a `Role:` key in Go source) constructs or assigns Principal.Role anywhere
// under cmd/ or internal/, and no package but internal/domain itself compares
// Role against OperatorRole or the literal "operator" -- every other
// consumer, including this package's own Validate, goes through
// Principal.IsOperator() instead. Both invariants are checked across the
// whole of cmd/ and internal/, not just this package, because either could
// otherwise be broken from any call site that imports internal/domain.
func TestOperatorRoleOnlyConstructedByConfigLoad(t *testing.T) {
	roleLiterals, roleAssignments, roleComparisons := 0, 0, 0
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			inDomain := strings.Contains(filepath.ToSlash(path), "/internal/domain/")
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.KeyValueExpr:
					if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Role" {
						roleLiterals++
						t.Logf("Role: composite-literal key in %s", path)
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Role" {
							roleAssignments++
							t.Logf(".Role assignment in %s", path)
						}
					}
				case *ast.BinaryExpr:
					if inDomain || (x.Op != token.EQL && x.Op != token.NEQ) {
						return true
					}
					if (isRoleSelector(x.X) && isOperatorLiteralOrConstant(x.Y)) ||
						(isRoleSelector(x.Y) && isOperatorLiteralOrConstant(x.X)) {
						roleComparisons++
						t.Logf("role comparison outside internal/domain in %s", path)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if roleLiterals != 0 || roleAssignments != 0 || roleComparisons != 0 {
		t.Fatalf("Principal.Role must be constructed only by config.Load's YAML decode and compared to \"operator\" only inside internal/domain: got %d Role: literals, %d .Role assignments, %d comparisons elsewhere",
			roleLiterals, roleAssignments, roleComparisons)
	}
}

func isRoleSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Role"
}

// isOperatorLiteralOrConstant reports whether e is the string literal
// "operator" or a qualified reference to domain.OperatorRole.
func isOperatorLiteralOrConstant(e ast.Expr) bool {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v, err := strconv.Unquote(lit.Value)
		return err == nil && v == "operator"
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "domain" && sel.Sel.Name == "OperatorRole" {
			return true
		}
	}
	return false
}
