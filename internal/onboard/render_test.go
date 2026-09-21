package onboard

import (
	"bytes"
	"encoding/json"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// TestRenderConfigBasic tests basic config rendering with default role name.
func TestRenderConfigBasic(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow:   1,
				AccountID:   "123456789012",
				AccountName: "Prod",
				Regions:     []string{"us-east-1", "eu-west-1"},
				RoleARN:     "",
				TenantID:    "tenant-a",
				Environment: "production",
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "account_id: \"123456789012\"") {
		t.Errorf("missing account_id in output")
	}
	if !strings.Contains(result, "role_arn: arn:aws:iam::123456789012:role/PlatformIpamReadOnly") {
		t.Errorf("missing generated role_arn")
	}
	if !strings.Contains(result, "us-east-1") || !strings.Contains(result, "eu-west-1") {
		t.Errorf("missing regions")
	}
	if !strings.Contains(result, "subject: \""+subjectPlaceholder+"\"") {
		t.Errorf("missing identity skeleton")
	}
	if !strings.Contains(result, "This is a fragment for review") {
		t.Errorf("missing header comment")
	}
}

// TestRenderConfigLeadingZeros tests that account IDs with leading zeros are preserved as quoted strings.
func TestRenderConfigLeadingZeros(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow:   1,
				AccountID:   "000123456789",
				Regions:     []string{"us-east-1"},
				TenantID:    "tenant-b",
				Environment: "staging",
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "\"000123456789\"") {
		t.Errorf("account ID with leading zeros not quoted as string: %s", result)
	}
}

// TestRenderConfigDefaultRole tests custom role name.
func TestRenderConfigDefaultRole(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow: 1,
				AccountID: "111122223333",
				Regions:   []string{"ap-southeast-1"},
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "CustomRole",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "role/CustomRole") {
		t.Errorf("custom role name not used: %s", result)
	}
}

// TestRenderConfigWithExplicitRoleARN tests that explicit role ARN is preserved.
func TestRenderConfigWithExplicitRoleARN(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow: 1,
				AccountID: "222233334444",
				Regions:   []string{"eu-central-1"},
				RoleARN:   "arn:aws:iam::222233334444:role/SpecialRole",
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "Ignored",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "role/SpecialRole") {
		t.Errorf("explicit role ARN not preserved")
	}
	if strings.Contains(result, "role/Ignored") {
		t.Errorf("default role name was used despite explicit ARN")
	}
}

// TestRenderConfigDefaultRegions tests fallback to default regions.
func TestRenderConfigDefaultRegions(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow: 1,
				AccountID: "333344445555",
				// No regions in row
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: []string{"us-west-2", "us-east-1"},
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "us-west-2") || !strings.Contains(result, "us-east-1") {
		t.Errorf("default regions not used")
	}
}

// TestRenderConfigNoRegionsError tests error when account has no regions.
func TestRenderConfigNoRegionsError(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow: 1,
				AccountID: "444455556666",
				// No regions in row or default
			},
		},
	}

	opts := ConfigOptions{
		RoleName: "PlatformIpamReadOnly",
		// No default regions
	}

	_, err := RenderConfig(tbl, opts)
	if err == nil {
		t.Errorf("expected error for account with no regions")
	}
	if !strings.Contains(err.Error(), "no regions") {
		t.Errorf("error message does not mention regions: %v", err)
	}
}

// TestRenderConfigWrongTableKind tests error for non-accounts table.
func TestRenderConfigWrongTableKind(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: []string{"us-east-1"},
	}

	_, err := RenderConfig(tbl, opts)
	if err == nil {
		t.Errorf("expected error for non-accounts table")
	}
}

// TestRenderConfigDeterministicOrdering tests that accounts are sorted by ID.
func TestRenderConfigDeterministicOrdering(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow: 1,
				AccountID: "999999999999",
				Regions:   []string{"us-east-1"},
			},
			{
				SourceRow: 2,
				AccountID: "111111111111",
				Regions:   []string{"us-east-1"},
			},
			{
				SourceRow: 3,
				AccountID: "555555555555",
				Regions:   []string{"us-east-1"},
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	idx111 := strings.Index(result, "\"111111111111\"")
	idx555 := strings.Index(result, "\"555555555555\"")
	idx999 := strings.Index(result, "\"999999999999\"")

	if idx111 < 0 || idx555 < 0 || idx999 < 0 {
		t.Errorf("not all account IDs found in output")
	}
	if !(idx111 < idx555 && idx555 < idx999) {
		t.Errorf("accounts are not sorted by ID")
	}
}

// TestRenderConfigIdentitiesSkeleton tests identity skeleton only for rows with tenant_id.
func TestRenderConfigIdentitiesSkeleton(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow:   1,
				AccountID:   "111111111111",
				Regions:     []string{"us-east-1"},
				TenantID:    "tenant-1",
				Environment: "prod",
			},
			{
				SourceRow:   2,
				AccountID:   "222222222222",
				Regions:     []string{"us-east-1"},
				TenantID:    "", // No tenant ID
				Environment: "dev",
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, "tenant-1") {
		t.Errorf("identity skeleton missing for account with tenant_id")
	}
	// The second account should not appear in identities
	lines := strings.Split(result, "\n")
	identStartIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "identities:") {
			identStartIdx = i
			break
		}
	}
	if identStartIdx >= 0 {
		identSection := strings.Join(lines[identStartIdx:], "\n")
		if strings.Contains(identSection, "222222222222") {
			t.Errorf("account without tenant_id should not have identity skeleton")
		}
	}
}

// TestRenderFixtureBasic tests basic fixture rendering.
func TestRenderFixtureBasic(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
		Networks: []NetworkRow{
			{
				SourceRow:  1,
				CIDR:       "10.0.0.0/16",
				AccountID:  "123456789012",
				Region:     "us-east-1",
				Type:       "vpc",
				ResourceID: "vpc-12345",
				Primary:    "true",
				State:      "available",
			},
		},
	}

	data, err := RenderFixture(tbl, "test-domain", "test-gen-1")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	var obs domain.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("failed to unmarshal observation: %v", err)
	}

	if obs.DomainID != "test-domain" {
		t.Errorf("domain_id mismatch: got %q", obs.DomainID)
	}
	if obs.Generation != "test-gen-1" {
		t.Errorf("generation mismatch: got %q", obs.Generation)
	}
	if !obs.Complete {
		t.Errorf("complete should be true")
	}
	if len(obs.Resources) != 1 {
		t.Errorf("expected 1 resource, got %d", len(obs.Resources))
	}

	res := obs.Resources[0]
	if res.AccountID != "123456789012" {
		t.Errorf("account_id mismatch: got %q", res.AccountID)
	}
	if res.Region != "us-east-1" {
		t.Errorf("region mismatch: got %q", res.Region)
	}
	if res.Type != "vpc" {
		t.Errorf("type mismatch: got %q", res.Type)
	}
	if res.CIDR != "10.0.0.0/16" {
		t.Errorf("CIDR mismatch: got %q", res.CIDR)
	}
	if res.State != "available" {
		t.Errorf("state mismatch: got %q", res.State)
	}
}

// TestRenderFixtureCompleteFieldExplicit tests that "complete" is explicitly present in JSON.
func TestRenderFixtureCompleteFieldExplicit(t *testing.T) {
	tbl := Table{
		Kind:     KindNetworks,
		Networks: []NetworkRow{},
	}

	data, err := RenderFixture(tbl, "test-domain", "test-gen-1")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	// Check that the raw JSON contains the "complete" key
	if !bytes.Contains(data, []byte(`"complete": true`)) && !bytes.Contains(data, []byte(`"complete":true`)) {
		t.Errorf("\"complete\" field not explicitly present in JSON output")
	}
}

// TestRenderFixtureRoundTrip tests that rendered fixture round-trips through json.Unmarshal.
func TestRenderFixtureRoundTrip(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
		Networks: []NetworkRow{
			{
				SourceRow:  1,
				CIDR:       "192.168.0.0/16",
				AccountID:  "987654321098",
				Region:     "eu-west-1",
				Type:       "vpc",
				ResourceID: "vpc-abcde",
				Primary:    "true",
				State:      "available",
				AZID:       "eu-west-1a",
				ParentID:   "",
			},
		},
	}

	data, err := RenderFixture(tbl, "domain-xyz", "gen-2")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	var obs domain.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if !obs.Complete {
		t.Errorf("Complete field should be true after round-trip")
	}
	if len(obs.Resources) != 1 {
		t.Fatalf("expected 1 resource")
	}
	if obs.Resources[0].ZoneID != "eu-west-1a" {
		t.Errorf("zone_id not preserved")
	}
}

// TestRenderFixtureMergeCIDRs tests merging of secondary CIDRs into a VPC.
func TestRenderFixtureMergeCIDRs(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
		Networks: []NetworkRow{
			{
				SourceRow:  1,
				CIDR:       "10.0.0.0/16",
				AccountID:  "111111111111",
				Region:     "us-east-1",
				Type:       "vpc",
				ResourceID: "vpc-primary",
				Primary:    "true",
			},
			{
				SourceRow:  2,
				CIDR:       "10.1.0.0/16",
				AccountID:  "111111111111",
				Region:     "us-east-1",
				Type:       "vpc",
				ResourceID: "vpc-primary",
				Primary:    "false",
			},
		},
	}

	data, err := RenderFixture(tbl, "domain", "gen")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	var obs domain.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(obs.Resources) != 1 {
		t.Errorf("expected 1 merged resource, got %d", len(obs.Resources))
	}

	res := obs.Resources[0]
	if res.CIDR != "10.0.0.0/16" {
		t.Errorf("primary CIDR should be 10.0.0.0/16")
	}
	if len(res.CIDRs) != 2 {
		t.Errorf("expected 2 CIDRs, got %d", len(res.CIDRs))
	}
}

// TestRenderFixtureSynthesizedID tests synthetic ID generation when resource_id is missing.
func TestRenderFixtureSynthesizedID(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
		Networks: []NetworkRow{
			{
				SourceRow:  1,
				CIDR:       "172.16.0.0/16",
				AccountID:  "222222222222",
				Region:     "ap-southeast-1",
				Type:       "vpc",
				ResourceID: "", // Missing resource ID
				Primary:    "true",
			},
		},
	}

	data, err := RenderFixture(tbl, "domain", "gen")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	var obs domain.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	res := obs.Resources[0]
	if res.ID == "" {
		t.Errorf("synthesized ID should not be empty")
	}
}

// TestRenderFixtureEmptyDomainIDError tests error when domainID is empty.
func TestRenderFixtureEmptyDomainIDError(t *testing.T) {
	tbl := Table{
		Kind:     KindNetworks,
		Networks: []NetworkRow{},
	}

	_, err := RenderFixture(tbl, "", "gen")
	if err == nil {
		t.Errorf("expected error for empty domainID")
	}
}

// TestRenderFixtureEmptyGenerationError tests error when generation is empty.
func TestRenderFixtureEmptyGenerationError(t *testing.T) {
	tbl := Table{
		Kind:     KindNetworks,
		Networks: []NetworkRow{},
	}

	_, err := RenderFixture(tbl, "domain", "")
	if err == nil {
		t.Errorf("expected error for empty generation")
	}
}

// TestRenderFixtureWrongTableKindError tests error for non-networks table.
func TestRenderFixtureWrongTableKindError(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
	}

	_, err := RenderFixture(tbl, "domain", "gen")
	if err == nil {
		t.Errorf("expected error for non-networks table")
	}
}

// TestRenderFixtureGoldenConfig tests against golden file for config output.
func TestRenderConfigGolden(t *testing.T) {
	tbl := Table{
		Kind: KindAccounts,
		Accounts: []AccountRow{
			{
				SourceRow:   1,
				AccountID:   "000000000001",
				AccountName: "Dev Account",
				Regions:     []string{"eu-central-1", "us-east-1"},
				TenantID:    "developer",
				Environment: "development",
			},
			{
				SourceRow:   2,
				AccountID:   "000000000002",
				AccountName: "Prod Account",
				Regions:     []string{"us-west-2"},
				RoleARN:     "arn:aws:iam::000000000002:role/CustomProdRole",
				TenantID:    "production",
				Environment: "production",
			},
		},
	}

	opts := ConfigOptions{
		RoleName:       "PlatformIpamReadOnly",
		DefaultRegions: nil,
	}

	data, err := RenderConfig(tbl, opts)
	if err != nil {
		t.Fatalf("RenderConfig failed: %v", err)
	}

	goldenPath := filepath.Join("testdata", "render_config.golden")
	golden := readOrUpdateGolden(t, goldenPath, data)

	if !bytes.Equal(data, golden) {
		t.Errorf("output does not match golden file. Got:\n%s\n\nExpected:\n%s", string(data), string(golden))
	}
}

// TestRenderFixtureGolden tests against golden file for fixture output.
func TestRenderFixtureGolden(t *testing.T) {
	tbl := Table{
		Kind: KindNetworks,
		Networks: []NetworkRow{
			{
				SourceRow:  1,
				CIDR:       "10.0.0.0/16",
				AccountID:  "000000000001",
				Region:     "eu-central-1",
				Type:       "vpc",
				ResourceID: "vpc-00001111",
				Primary:    "true",
				State:      "available",
			},
			{
				SourceRow:  2,
				CIDR:       "10.0.1.0/24",
				AccountID:  "000000000001",
				Region:     "eu-central-1",
				Type:       "subnet",
				ResourceID: "subnet-22223333",
				ParentID:   "vpc-00001111",
				AZID:       "eu-central-1a",
				Primary:    "true",
				State:      "available",
			},
		},
	}

	data, err := RenderFixture(tbl, "local-development", "dev-1")
	if err != nil {
		t.Fatalf("RenderFixture failed: %v", err)
	}

	goldenPath := filepath.Join("testdata", "render_fixture.golden")
	golden := readOrUpdateGolden(t, goldenPath, data)

	if !bytes.Equal(data, golden) {
		t.Errorf("output does not match golden file. Got:\n%s\n\nExpected:\n%s", string(data), string(golden))
	}
}

// readOrUpdateGolden returns the golden file's content. It rewrites the file
// only when UPDATE_GOLDEN=1 is set: a missing golden file is a failure, not
// an invitation to bless whatever the code currently produces.
func readOrUpdateGolden(t *testing.T, path string, data []byte) []byte {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create testdata directory: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write golden file: %v", err)
		}
		return data
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run with UPDATE_GOLDEN=1 to create it): %v", err)
	}
	return content
}

// The fragment must be YAML that decodes into the shapes the service loads.
// A golden-file comparison alone cannot show that: it faithfully preserved
// "regions: [a b]", which YAML reads as a single string.
func TestRenderConfigOutputIsLoadableYAML(t *testing.T) {
	table := Table{Kind: KindAccounts, Accounts: []AccountRow{{
		AccountID: "000123456789", TenantID: "orders", Environment: "prod",
		Regions: []string{"eu-central-1", "eu-west-1"},
	}}}
	out, err := RenderConfig(table, ConfigOptions{})
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	var doc struct {
		CloudCoverage []struct {
			AccountID string   `yaml:"account_id"`
			RoleARN   string   `yaml:"role_arn"`
			Regions   []string `yaml:"regions"`
		} `yaml:"cloud_coverage"`
		EligibleAccounts []string `yaml:"eligible_accounts"`
		Identities       []struct {
			Subject  string   `yaml:"subject"`
			TenantID string   `yaml:"tenant_id"`
			Accounts []string `yaml:"accounts"`
			Regions  []string `yaml:"regions"`
		} `yaml:"identities"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered fragment is not valid YAML: %v\n%s", err, out)
	}
	if got := doc.CloudCoverage[0].AccountID; got != "000123456789" {
		t.Errorf("account id = %q, leading zeros lost", got)
	}
	if got := doc.EligibleAccounts[0]; got != "000123456789" {
		t.Errorf("eligible account = %q", got)
	}
	if got := len(doc.Identities[0].Regions); got != 2 {
		t.Errorf("identity regions decoded as %d item(s) %q, want 2", got, doc.Identities[0].Regions)
	}
	if got := len(doc.CloudCoverage[0].Regions); got != 2 {
		t.Errorf("coverage regions decoded as %d item(s), want 2", got)
	}
	if got := doc.Identities[0].Subject; got != subjectPlaceholder {
		t.Errorf("subject = %q; the import must not invent a subject", got)
	}
}
