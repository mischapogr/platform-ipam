package onboard

import (
	"strings"
	"testing"
)

// Every alias design section 3 lists must resolve to the same canonical
// column as its plain form, case-, punctuation- and whitespace-insensitively.
func TestResolveHeadersAliasTable(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Account ID", colAccountID},
		{"Account-Nr.", colAccountID},
		{"AccountId", colAccountID},
		{"AWS Konto", colAccountID},
		{"AWS Account", colAccountID},
		{"Account Name", colAccountName},
		{"CIDR", colCIDR},
		{"CIDR Block", colCIDR},
		{"IP Range", colCIDR},
		{"Netz", colCIDR},
		{"Network", colCIDR},
		{"Prefix", colCIDR},
		{"Region", colRegion},
		{"VPC ID", colResourceID},
		{"Subnet ID", colResourceID},
		{"Resource ID", colResourceID},
		{"AZ", colAZID},
		{"AZ ID", colAZID},
		{"Availability Zone ID", colAZID},
		{"Env", colEnvironment},
		{"Environment", colEnvironment},
		{"Tenant", colTenantID},
		{"Owner", colOwner},
		{"Description", colDescription},
		{"Notes", colDescription},
		{"Start", colStartAddress},
		{"Start Address", colStartAddress},
		{"End", colEndAddress},
		{"End Address", colEndAddress},
		{"Role ARN", colRoleARN},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			// Pair the alias with "Account ID" and "Region" so every case
			// resolves to a real table kind regardless of which column it
			// names: a CIDR alias next to an account but without a region is
			// refused by design.
			_, columns, err := ResolveHeaders([]string{"Account ID", tc.header, "Region"})
			if err != nil {
				t.Fatalf("ResolveHeaders: %v", err)
			}
			if columns[1] != tc.want {
				t.Errorf("header %q resolved to %q, want %q", tc.header, columns[1], tc.want)
			}
		})
	}
}

// Table kind is decided by which required columns resolve (design section
// 3): networks needs cidr+account_id+region; ranges needs cidr, or
// start_address and end_address; accounts needs only account_id.
func TestResolveHeadersDecidesKind(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    TableKind
	}{
		{"accounts", []string{"Account ID", "Account Name", "Owner"}, KindAccounts},
		{"networks", []string{"CIDR", "Account ID", "Region", "VPC ID"}, KindNetworks},
		{"ranges by cidr", []string{"IP Range", "Description"}, KindRanges},
		{"ranges by start/end", []string{"Start Address", "End Address", "Owner"}, KindRanges},
		{"german aliases", []string{"AWS Konto", "Netz", "Region"}, KindNetworks},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, _, err := ResolveHeaders(tc.headers)
			if err != nil {
				t.Fatalf("ResolveHeaders(%v): %v", tc.headers, err)
			}
			if kind != tc.want {
				t.Errorf("kind = %q, want %q", kind, tc.want)
			}
		})
	}
}

// A header row that satisfies no table's required columns must fail with a
// message naming the headers that were actually seen, so an operator can
// tell what was misread.
func TestResolveHeadersRequiredColumnMissingNamesSeenHeaders(t *testing.T) {
	_, _, err := ResolveHeaders([]string{"Account Name", "Owner", "Notes"})
	if err == nil {
		t.Fatal("ResolveHeaders: want an error, got nil")
	}
	for _, want := range []string{"Account Name", "Owner", "Notes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name seen header %q", err, want)
		}
	}
}

// An unrecognized header must be kept, not dropped, so Normalize can fold
// its value into the row's description.
func TestResolveHeadersKeepsUnknownColumns(t *testing.T) {
	_, columns, err := ResolveHeaders([]string{"Account ID", "Cost Center"})
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	if columns[1] != "Cost Center" {
		t.Errorf("columns[1] = %q, want the original unrecognized header %q", columns[1], "Cost Center")
	}
}

func TestNormalizeHeaderKeyStripsCaseAndPunctuation(t *testing.T) {
	cases := map[string]string{
		"Account ID":    "accountid",
		"Account-Nr.":   "accountnr",
		"  Role  ARN  ": "rolearn",
	}
	for in, want := range cases {
		if got := normalizeHeaderKey(in); got != want {
			t.Errorf("normalizeHeaderKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A networks table that lost its region column must be refused, not quietly
// downgraded to anonymous ranges with the account pushed into a description.
func TestResolveHeadersRefusesNetworksWithoutRegion(t *testing.T) {
	_, _, err := ResolveHeaders([]string{"Account ID", "CIDR", "Name"})
	if err == nil {
		t.Fatal("ResolveHeaders accepted cidr+account_id without region")
	}
	for _, want := range []string{"region", "Account ID", "CIDR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Without the account column the same table is a legitimate ranges table.
	kind, _, err := ResolveHeaders([]string{"CIDR", "Name"})
	if err != nil || kind != KindRanges {
		t.Errorf("ResolveHeaders(cidr only) = %q, %v; want ranges", kind, err)
	}
}
