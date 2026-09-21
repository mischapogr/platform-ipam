package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// ConfigOptions provides configuration for RenderConfig.
type ConfigOptions struct {
	RoleName       string   // defaults to "PlatformIpamReadOnly"
	DefaultRegions []string // fallback regions if a row has none
}

// RenderConfig renders an ACCOUNTS table to YAML fragments for human review.
// Output includes: cloud_coverage list, eligible_accounts list, and identities skeleton.
// The output begins with comment lines about the fragment and coverage_generation.
// Account IDs are kept as 12-digit quoted strings (never YAML integers).
// Returns error if table is not ACCOUNTS or if an account has no regions.
func RenderConfig(t Table, opts ConfigOptions) ([]byte, error) {
	if t.Kind != KindAccounts {
		return nil, fmt.Errorf("RenderConfig requires an accounts table, got %s", t.Kind)
	}

	if opts.RoleName == "" {
		opts.RoleName = "PlatformIpamReadOnly"
	}

	var buf bytes.Buffer

	// Write header comments
	buf.WriteString("# This is a fragment for review. Nothing has been written to the repository.\n")
	buf.WriteString("# Changing cloud_coverage requires advancing coverage_generation, which invalidates absence evidence.\n")
	buf.WriteString("# IPAM_IDENTITY_FILE replaces the identity list wholesale, so a partial identities file silently removes existing principals.\n")
	buf.WriteString("\n")

	// Collect accounts and sort by ID for deterministic output
	type accountInfo struct {
		id      string
		row     AccountRow
		roleARN string
		regions []string
	}
	var accounts []accountInfo
	for _, row := range t.Accounts {
		regions := row.Regions
		if len(regions) == 0 {
			regions = opts.DefaultRegions
		}
		if len(regions) == 0 {
			return nil, fmt.Errorf("account %s has no regions and no default provided", row.AccountID)
		}
		sort.Strings(regions)

		roleARN := row.RoleARN
		if roleARN == "" {
			roleARN = fmt.Sprintf("arn:aws:iam::%s:role/%s", row.AccountID, opts.RoleName)
		}

		accounts = append(accounts, accountInfo{
			id:      row.AccountID,
			row:     row,
			roleARN: roleARN,
			regions: regions,
		})
	}

	// Sort by account ID
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].id < accounts[j].id
	})

	// Write cloud_coverage
	buf.WriteString("cloud_coverage:\n")
	for _, acc := range accounts {
		buf.WriteString(fmt.Sprintf("  - account_id: \"%s\"\n", acc.id))
		buf.WriteString(fmt.Sprintf("    role_arn: %s\n", acc.roleARN))
		buf.WriteString("    regions:\n")
		for _, region := range acc.regions {
			buf.WriteString(fmt.Sprintf("      - %s\n", region))
		}
	}

	// Write eligible_accounts
	buf.WriteString("\neligible_accounts:\n")
	for _, acc := range accounts {
		buf.WriteString(fmt.Sprintf("  - \"%s\"\n", acc.id))
	}

	// Identity skeletons, only for rows that name a tenant. The subject is
	// deliberately a placeholder: an accounts table says which tenant owns an
	// account, never which authenticated subject may act for it, and guessing
	// one here would hand a principal to whoever merges the fragment unread.
	wroteHeader := false
	for _, acc := range accounts {
		if acc.row.TenantID == "" {
			continue
		}
		if !wroteHeader {
			buf.WriteString("\nidentities:\n")
			wroteHeader = true
		}
		buf.WriteString(fmt.Sprintf("  - subject: %q\n", subjectPlaceholder))
		buf.WriteString(fmt.Sprintf("    tenant_id: %q\n", acc.row.TenantID))
		buf.WriteString(fmt.Sprintf("    accounts: [%q]\n", acc.id))
		if acc.row.Environment != "" {
			buf.WriteString(fmt.Sprintf("    environments: [%q]\n", acc.row.Environment))
		}
		buf.WriteString(fmt.Sprintf("    regions: %s\n", yamlFlowList(acc.regions)))
	}

	return buf.Bytes(), nil
}

// subjectPlaceholder marks the one identity field the import cannot know.
const subjectPlaceholder = "REPLACE_WITH_AUTHENTICATED_SUBJECT"

// yamlFlowList renders strings as a YAML flow sequence. Formatting a slice with
// %v yields "[a b]", which YAML reads as ONE string, not two items.
func yamlFlowList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// RenderFixture renders a NETWORKS table to JSON as a domain.Observation fixture.
// Output includes one Resource per VPC/subnet row with explicit "complete": true.
// Secondary CIDRs of the same VPC are merged into the primary's cidrs list.
// Returns error if domainID or generation is empty or if table is not NETWORKS.
func RenderFixture(t Table, domainID, generation string) ([]byte, error) {
	if t.Kind != KindNetworks {
		return nil, fmt.Errorf("RenderFixture requires a networks table, got %s", t.Kind)
	}

	if domainID == "" {
		return nil, fmt.Errorf("domainID must not be empty")
	}
	if generation == "" {
		return nil, fmt.Errorf("generation must not be empty")
	}

	// Group rows by resource_id (or synthesized ID) to merge secondary CIDRs
	type resourceInfo struct {
		row           NetworkRow
		cidrs         []string
		primary       string
		synthesizedID string
	}
	resources := make(map[string]*resourceInfo)

	for _, row := range t.Networks {
		// Use resource_id if present, otherwise synthesize
		resID := row.ResourceID
		if resID == "" {
			// Synthesize: vpc-import-<n> pattern
			resID = fmt.Sprintf("vpc-import-%d", len(resources))
		}

		if _, exists := resources[resID]; !exists {
			resources[resID] = &resourceInfo{
				row:           row,
				cidrs:         []string{},
				synthesizedID: resID,
			}
		}

		info := resources[resID]
		// Add CIDR to the list
		if row.CIDR != "" {
			info.cidrs = append(info.cidrs, row.CIDR)
		}

		// Track primary CIDR
		if row.Primary == "true" || row.Primary == "TRUE" || row.Primary == "1" {
			info.primary = row.CIDR
		}
	}

	// Build resources array
	var resArray []domain.Resource
	for _, info := range resources {
		res := domain.Resource{
			AccountID: info.row.AccountID,
			Region:    info.row.Region,
			Type:      info.row.Type,
			ID:        info.row.ResourceID,
			CIDR:      info.primary,
			CIDRs:     info.cidrs,
			ParentID:  info.row.ParentID,
			ZoneID:    info.row.AZID,
			State:     "available",
		}
		// Use synthesized ID if resource_id was empty
		if res.ID == "" {
			res.ID = info.synthesizedID
		}
		if res.State == "" {
			res.State = "available"
		}
		resArray = append(resArray, res)
	}

	// Sort resources for deterministic output: by account_id, region, type, id
	sort.Slice(resArray, func(i, j int) bool {
		if resArray[i].AccountID != resArray[j].AccountID {
			return resArray[i].AccountID < resArray[j].AccountID
		}
		if resArray[i].Region != resArray[j].Region {
			return resArray[i].Region < resArray[j].Region
		}
		if resArray[i].Type != resArray[j].Type {
			return resArray[i].Type < resArray[j].Type
		}
		return resArray[i].ID < resArray[j].ID
	})

	obs := domain.Observation{
		DomainID:   domainID,
		Generation: generation,
		Complete:   true,
		Resources:  resArray,
	}

	// Marshal to JSON with proper formatting
	data, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal observation: %w", err)
	}

	// Ensure "complete" is explicitly present in the output
	// by re-marshaling with a wrapper that forces its inclusion
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["complete"] = true

	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal with complete field: %w", err)
	}

	return data, nil
}
