package onboardcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// driftConfigYAML is a valid pools configuration (internal/config's
// Validate) shaped for drift's rules that ARE reachable through the real
// config.Load path:
//
//   - domain "d1" (VRF 7), cloud_coverage covers account 111111111111
//     (region eu-central-1) and 222222222222 (region eu-central-1) --
//     222222222222 is covered but will have no plugin object, to exercise
//     unknown-to-plugin.
//   - pool "pool1" in d1, eligible for 111111111111 only (config.Validate
//     requires every eligible account to already have coverage, so
//     eligible-without-coverage cannot be produced this way -- it is tested
//     directly against planDrift instead, see TestPlanDriftEligibleWithoutCoverage).
//   - one identity mapped to 333333333333, which no domain covers, to
//     exercise identity-without-coverage.
const driftConfigYAML = `schema_version: 1
policy_version: "test-1"

overlap_domains:
  - id: d1
    description: test domain
    coverage_generation: "g1"
    inventory_backend:
      type: netbox
      vrf_id: 7
      require_unique_prefixes: true
    cloud_coverage:
      - account_id: "111111111111"
        role_arn: arn:aws:iam::111111111111:role/PlatformIpamReadOnly
        regions: [eu-central-1]
      - account_id: "222222222222"
        role_arn: arn:aws:iam::222222222222:role/PlatformIpamReadOnly
        regions: [eu-central-1]

pools:
  - id: pool1
    overlap_domain: d1
    cidr: 10.0.0.0/16
    address_family: ipv4
    scope: vpc
    environment: prod
    region: eu-central-1
    eligible_tenants: [team-a]
    eligible_accounts: ["111111111111"]
    allowed_prefix_lengths: [20, 22]
    max_unreclaimed_allocations_per_tenant: 8
    inventory_backend:
      parent_prefix_id: 100

subnet_policy:
  require_parent_scope: vpc
  inherit_parent_target: true
  allowed_prefix_lengths: [24, 26]
  max_unreclaimed_children: 12
  allow_nested_subnets: false

lifecycle:
  reservation_expiry_enabled: false
  reservation_age_alert_hours: 24
  quarantine_hours: 168
  required_complete_absence_scans: 2
  minimum_scan_spacing_seconds: 300
  maximum_observation_age_seconds: 600

reconciliation:
  full_scan_interval_seconds: 300
  include_untagged_resources: true
  include_all_vpc_cidr_associations: true
  unknown_coverage_blocks_reuse: true

ui:
  inventory_links_enabled: false

identities:
  - subject: someone
    tenant_id: team-a
    accounts: ["333333333333"]
    environments: [prod]
    regions: [eu-central-1]
`

// driftAccountsStub is a minimal fake of the plugin's AWSAccount endpoint.
// onboard drift never calls Snapshot, so unlike netboxStub (onboardcmd_test.go)
// this stub answers only /api/plugins/aws-vpc/aws-accounts/ and 404s
// everything else, which doubles as proof that drift never asks for anything
// beyond that one path.
type driftAccountsStub struct {
	accounts []map[string]any
	notFound bool
	fail500  bool
	requests []string
}

func (s *driftAccountsStub) nonGETRequests() []string {
	var out []string
	for _, r := range s.requests {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}
	return out
}

func (s *driftAccountsStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/api/plugins/aws-vpc/aws-accounts/" {
			http.NotFound(w, r)
			return
		}
		if s.notFound {
			http.NotFound(w, r)
			return
		}
		if s.fail500 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(page(toAny(s.accounts)))
	}))
}

func driftTestEnv(t *testing.T, netboxURL string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "pools.yaml"), driftConfigYAML)
	t.Setenv("IPAM_CONFIG_FILE", cfgPath)
	t.Setenv("IPAM_IDENTITY_FILE", "")
	t.Setenv("IPAM_ENVIRONMENT", "development")
	t.Setenv("IPAM_NETBOX_URL", netboxURL)
	t.Setenv("IPAM_NETBOX_TOKEN", "test-token")
}

func pluginAccount(id int, accountID, status string) map[string]any {
	return map[string]any{"id": id, "account_id": accountID, "name": "acct",
		"status": map[string]any{"value": status, "label": status}}
}

func findingsByRule(report DriftReport, rule string) []DriftFinding {
	var out []DriftFinding
	for _, f := range report.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

func runDriftCmd(t *testing.T, args []string) (int, DriftReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	var report DriftReport
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("stdout is not a DriftReport: %v\n%s", err, stdout.String())
		}
	}
	return code, report, stderr.String()
}

// --- one test per rule id reachable through the command ---

func TestDriftUncoveredAccountIsError(t *testing.T) {
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"), // covered: no finding
		pluginAccount(2, "999999999999", "ACTIVE"), // not covered: uncovered-account
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d (validation); stderr=%s", code, ExitValidation, stderr)
	}
	found := findingsByRule(report, DriftRuleUncoveredAccount)
	if len(found) != 1 || found[0].AccountID != "999999999999" || found[0].Level != driftLevelError {
		t.Fatalf("uncovered-account findings = %+v", found)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftUncoveredAccountInactiveStatusIsInfoNotError(t *testing.T) {
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"),
		pluginAccount(2, "999999999999", "INACTIVE"),
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (an info finding must not fail the command); stderr=%s", code, ExitOK, stderr)
	}
	found := findingsByRule(report, DriftRuleUncoveredAccount)
	if len(found) != 1 || found[0].AccountID != "999999999999" || found[0].Level != driftLevelInfo {
		t.Fatalf("uncovered-account findings = %+v, want exactly one info-level finding for 999999999999", found)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftUnknownToPluginIsWarning(t *testing.T) {
	// driftConfigYAML covers both 111111111111 and 222222222222; only
	// 111111111111 gets a plugin object here, so 222222222222 is
	// unknown-to-plugin.
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"),
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (a warning must not fail the command); stderr=%s", code, ExitOK, stderr)
	}
	found := findingsByRule(report, DriftRuleUnknownToPlugin)
	if len(found) != 1 || found[0].AccountID != "222222222222" || found[0].Level != driftLevelWarning {
		t.Fatalf("unknown-to-plugin findings = %+v", found)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftIdentityWithoutCoverageIsWarning(t *testing.T) {
	// driftConfigYAML has one identity targeting 333333333333, which no
	// domain covers.
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"),
		pluginAccount(2, "222222222222", "ACTIVE"),
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr)
	}
	found := findingsByRule(report, DriftRuleIdentityWithoutCoverage)
	if len(found) != 1 || found[0].AccountID != "333333333333" || found[0].Level != driftLevelWarning {
		t.Fatalf("identity-without-coverage findings = %+v", found)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftInvalidPluginAccountIDIsReportedNotCrashed(t *testing.T) {
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"),
		{"id": 2, "account_id": nil, "name": "nulled", "status": map[string]any{"value": "ACTIVE", "label": "Active"}},
		{"id": 3, "account_id": "12345", "name": "tooshort", "status": map[string]any{"value": "ACTIVE", "label": "Active"}},
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr)
	}
	found := findingsByRule(report, DriftRuleInvalidPluginAccountID)
	if len(found) != 2 {
		t.Fatalf("invalid-plugin-account-id findings = %+v, want exactly 2 (null and non-12-digit)", found)
	}
	for _, f := range found {
		if f.Level != driftLevelWarning {
			t.Fatalf("finding %+v: level = %q, want %q", f, f.Level, driftLevelWarning)
		}
	}
	// Neither malformed entry may leak into uncovered-account: comparing a
	// null or too-short id against configuration would be meaningless.
	if uncov := findingsByRule(report, DriftRuleUncoveredAccount); len(uncov) != 0 {
		t.Fatalf("uncovered-account findings = %+v, want none (malformed ids must be excluded, not compared)", uncov)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

// TestPlanDriftEligibleWithoutCoverage exercises DriftRuleEligibleWithoutCoverage
// directly against the pure planDrift function. internal/config.Validate
// already refuses to load a configuration where a pool's eligible_accounts
// contains an account absent from the domain's cloud_coverage (a coverage
// cell must exist for the pool's own region), so this rule cannot be
// produced by any config.Load-backed test -- the work plan calls it "defence
// in depth" for a config loaded some other way, and this test proves the
// comparison itself is correct even though `onboard drift`'s own command path
// can never reach it with a config it loaded itself.
func TestPlanDriftEligibleWithoutCoverage(t *testing.T) {
	cfg := domain.Config{
		Domains: []domain.Domain{{ID: "d1", CloudCoverage: []domain.Cell{
			{AccountID: "111111111111", RoleARN: "arn:aws:iam::111111111111:role/x", Regions: []string{"eu-central-1"}},
		}}},
		Pools: []domain.Pool{
			{ID: "pool1", DomainID: "d1", EligibleAccounts: []string{"111111111111", "444444444444"}},
		},
	}
	d := cfg.Domains[0]
	report := planDrift(cfg, d, nil)

	found := findingsByRule(report, DriftRuleEligibleWithoutCoverage)
	if len(found) != 1 || found[0].AccountID != "444444444444" || found[0].Level != driftLevelError {
		t.Fatalf("eligible-without-coverage findings = %+v", found)
	}
	if !report.HasErrors() {
		t.Fatal("report should have errors")
	}
}

// --- clean result, plugin unavailable, NetBox broken ---

func TestDriftCleanResultExitsZeroWithEmptyFindings(t *testing.T) {
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "111111111111", "ACTIVE"),
		pluginAccount(2, "222222222222", "ACTIVE"),
	}}
	server := stub.server(t)
	defer server.Close()
	// A config without the stray identity keeps this scenario genuinely
	// clean: driftConfigYAML's identity targets an uncovered account on
	// purpose for TestDriftIdentityWithoutCoverageIsWarning, so this test uses
	// its own minimal, fully-covered configuration.
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "pools.yaml"), strings.ReplaceAll(driftConfigYAML, `identities:
  - subject: someone
    tenant_id: team-a
    accounts: ["333333333333"]
    environments: [prod]
    regions: [eu-central-1]
`, ""))
	t.Setenv("IPAM_CONFIG_FILE", cfgPath)
	t.Setenv("IPAM_IDENTITY_FILE", "")
	t.Setenv("IPAM_ENVIRONMENT", "development")
	t.Setenv("IPAM_NETBOX_URL", server.URL)
	t.Setenv("IPAM_NETBOX_TOKEN", "test-token")

	code, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Findings = %+v, want empty (fully covered, fully known to the plugin)", report.Findings)
	}
	if report.Domain != "d1" || report.Counts.PluginAccounts != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftPluginNotInstalledExitsAdapterWithNoStdoutReport(t *testing.T) {
	stub := &driftAccountsStub{notFound: true}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"drift", "--domain", "d1"}, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want %d (adapter); stderr=%s", code, ExitAdapter, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("drift printed a report despite the plugin being unavailable: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "NETBOX_AWS_PLUGIN.md") {
		t.Fatalf("stderr does not name docs/NETBOX_AWS_PLUGIN.md: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "compose.netbox-plugin.yaml") {
		t.Fatalf("stderr does not name the overlay: %s", stderr.String())
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

func TestDriftNetBoxServerErrorExitsAdapter(t *testing.T) {
	stub := &driftAccountsStub{fail500: true}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"drift", "--domain", "d1"}, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want %d (adapter); stderr=%s", code, ExitAdapter, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("drift printed a report despite NetBox failing: %s", stdout.String())
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

// --- usage ---

func TestDriftRequiresDomainFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"drift"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (missing --domain)", code, ExitUsage)
	}
}

func TestDriftUnknownDomainIsUsageError(t *testing.T) {
	stub := &driftAccountsStub{}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"drift", "--domain", "does-not-exist"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (unknown domain); stderr=%s", code, ExitUsage, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("drift printed a report for an unknown domain: %s", stdout.String())
	}
	if len(stub.nonGETRequests()) != 0 {
		t.Fatalf("drift made non-GET request(s): %v", stub.nonGETRequests())
	}
}

// deterministic sort order sanity check.
func TestDriftFindingsAreSortedByRuleThenAccountID(t *testing.T) {
	stub := &driftAccountsStub{accounts: []map[string]any{
		pluginAccount(1, "999999999999", "ACTIVE"),
		pluginAccount(2, "888888888888", "ACTIVE"),
	}}
	server := stub.server(t)
	defer server.Close()
	driftTestEnv(t, server.URL)

	_, report, stderr := runDriftCmd(t, []string{"drift", "--domain", "d1"})
	if len(report.Findings) < 2 {
		t.Fatalf("expected at least 2 findings, got %+v; stderr=%s", report.Findings, stderr)
	}
	for i := 1; i < len(report.Findings); i++ {
		prev, cur := report.Findings[i-1], report.Findings[i]
		if prev.Rule > cur.Rule || (prev.Rule == cur.Rule && prev.AccountID > cur.AccountID) {
			t.Fatalf("findings not sorted at index %d: %+v then %+v", i, prev, cur)
		}
	}
}
