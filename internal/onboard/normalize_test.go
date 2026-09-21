package onboard

import (
	"strings"
	"testing"
	"time"
)

// resolve is a test helper: it calls ResolveHeaders and fails the test on
// error, so row-normalization tests can build a real columns map instead of
// hand-writing one.
func resolve(t *testing.T, headers []string) (TableKind, map[int]string) {
	t.Helper()
	kind, columns, err := ResolveHeaders(headers)
	if err != nil {
		t.Fatalf("ResolveHeaders(%v): %v", headers, err)
	}
	return kind, columns
}

func diagLevels(diags []Diagnostic) []Level {
	levels := make([]Level, len(diags))
	for i, d := range diags {
		levels[i] = d.Level
	}
	return levels
}

// --- design section 4, row: NBSP, zero-width space, BOM, smart quotes ---

func TestCleanCellStripsInvisibleCharactersAndStraightensQuotes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"BOM and NBSP", bom + " Hello World ", "Hello World"},
		{"zero-width characters", "Zero​Width‌‍Space", "ZeroWidthSpace"},
		{"smart quotes", "‘quoted’ and “double”", "'quoted' and \"double\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanCell(tc.in); got != tc.want {
				t.Errorf("cleanCell(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeStripsInvisibleCharactersThroughARow(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID", "Account Name"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "“Acme Corp”"},
	})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(table.Accounts))
	}
	if got, want := table.Accounts[0].AccountName, "\"Acme Corp\""; got != want {
		t.Errorf("AccountName = %q, want %q", got, want)
	}
}

// --- design section 4, row: several CIDRs in one cell ---

func TestNormalizeSplitsMultiCIDRCellIntoOneRowEach(t *testing.T) {
	kind, columns := resolve(t, []string{"CIDR", "Account ID", "Region"})
	table := Normalize(kind, columns, [][]string{
		{"10.1.0.0/16, 10.2.0.0/16\n10.3.0.0/16;10.4.0.0/16", "123456789012", "eu-central-1"},
	})
	want := []string{"10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/16", "10.4.0.0/16"}
	if len(table.Networks) != len(want) {
		t.Fatalf("Networks = %d rows, want %d: %+v", len(table.Networks), len(want), table.Networks)
	}
	for i, w := range want {
		if table.Networks[i].CIDR != w {
			t.Errorf("row %d CIDR = %q, want %q", i, table.Networks[i].CIDR, w)
		}
		if table.Networks[i].SourceRow != 1 {
			t.Errorf("row %d SourceRow = %d, want 1 (all split from the same source row)", i, table.Networks[i].SourceRow)
		}
	}
}

// --- design section 4, row: decoration ---

func TestNormalizeExtractsDecoratedCIDR(t *testing.T) {
	cases := []struct {
		name        string
		cell        string
		wantCIDR    string
		wantInDescr string
	}{
		{"trailing annotation", "10.1.0.0/16 (prod)", "10.1.0.0/16", "prod"},
		{"leading label", "VPC: 10.1.0.0/16", "10.1.0.0/16", "VPC"},
	}
	kind, columns := resolve(t, []string{"CIDR", "Account ID", "Region"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := Normalize(kind, columns, [][]string{{tc.cell, "123456789012", "eu-central-1"}})
			if len(table.Networks) != 1 {
				t.Fatalf("Networks = %d rows, want 1: diagnostics=%+v", len(table.Networks), table.Diagnostics)
			}
			row := table.Networks[0]
			if row.CIDR != tc.wantCIDR {
				t.Errorf("CIDR = %q, want %q", row.CIDR, tc.wantCIDR)
			}
			if !strings.Contains(row.Description, tc.wantInDescr) {
				t.Errorf("Description = %q, want it to contain %q", row.Description, tc.wantInDescr)
			}
		})
	}
}

// --- design section 4, row: non-canonical CIDR ---

func TestNormalizeRejectsNonCanonicalCIDR(t *testing.T) {
	kind, columns := resolve(t, []string{"CIDR", "Account ID", "Region"})
	table := Normalize(kind, columns, [][]string{
		{"10.1.2.3/16", "123456789012", "eu-central-1"},
	})
	if len(table.Networks) != 0 {
		t.Fatalf("Networks = %d rows, want 0 (non-canonical CIDR must not be silently masked): %+v", len(table.Networks), table.Networks)
	}
	if len(table.Diagnostics) != 1 || table.Diagnostics[0].Level != LevelError {
		t.Fatalf("Diagnostics = %+v, want exactly one error", table.Diagnostics)
	}
	if table.Diagnostics[0].Row != 1 || table.Diagnostics[0].Column != "cidr" {
		t.Errorf("Diagnostic = %+v, want Row=1 Column=cidr", table.Diagnostics[0])
	}
}

// --- design section 4, row: Excel dropped leading zeros ---

func TestNormalizePadsAccountIDThatLostLeadingZeros(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID"})
	table := Normalize(kind, columns, [][]string{{"12345678"}})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(table.Accounts))
	}
	if got, want := table.Accounts[0].AccountID, "000012345678"; got != want {
		t.Errorf("AccountID = %q, want %q", got, want)
	}
	if len(table.Diagnostics) != 1 || table.Diagnostics[0].Level != LevelWarning {
		t.Fatalf("Diagnostics = %+v, want exactly one warning", table.Diagnostics)
	}
	if table.Diagnostics[0].Row != 1 {
		t.Errorf("Diagnostic.Row = %d, want 1 (must name the row)", table.Diagnostics[0].Row)
	}
}

// --- design section 4, row: Excel scientific notation ---

func TestNormalizeRejectsScientificNotationAccountID(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID"})
	table := Normalize(kind, columns, [][]string{{"1.23457E+11"}})
	if len(table.Accounts) != 0 {
		t.Fatalf("Accounts = %d rows, want 0 (lost digits must not be guessed): %+v", len(table.Accounts), table.Accounts)
	}
	if len(table.Diagnostics) != 1 || table.Diagnostics[0].Level != LevelError {
		t.Fatalf("Diagnostics = %+v, want exactly one error", table.Diagnostics)
	}
}

// --- design section 4, row: account id with dashes or spaces ---

func TestNormalizeStripsAccountIDSeparators(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID"})
	table := Normalize(kind, columns, [][]string{{"1234-5678-9012"}})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(table.Accounts))
	}
	if got, want := table.Accounts[0].AccountID, "123456789012"; got != want {
		t.Errorf("AccountID = %q, want %q", got, want)
	}
	if len(table.Diagnostics) != 0 {
		t.Errorf("Diagnostics = %+v, want none (separator removal is not a warning)", table.Diagnostics)
	}
}

// --- design section 4, row: range written as "start - end" ---

func TestNormalizeParsesDashSeparatedRange(t *testing.T) {
	kind, columns := resolve(t, []string{"IP Range", "Description"})
	if kind != KindRanges {
		t.Fatalf("kind = %q, want %q", kind, KindRanges)
	}
	table := Normalize(kind, columns, [][]string{
		{"10.0.0.1 - 10.0.0.50", "office network"},
	})
	if len(table.Ranges) != 1 {
		t.Fatalf("Ranges = %d rows, want 1: diagnostics=%+v", len(table.Ranges), table.Diagnostics)
	}
	row := table.Ranges[0]
	if row.StartAddress != "10.0.0.1" || row.EndAddress != "10.0.0.50" {
		t.Errorf("row = %+v, want start=10.0.0.1 end=10.0.0.50", row)
	}
	if row.CIDR != "" {
		t.Errorf("CIDR = %q, want empty for a start/end range", row.CIDR)
	}
}

// --- design section 4, row: IPv6 ---

func TestNormalizeSkipsIPv6WithWarning(t *testing.T) {
	t.Run("networks cidr column", func(t *testing.T) {
		kind, columns := resolve(t, []string{"CIDR", "Account ID", "Region"})
		table := Normalize(kind, columns, [][]string{
			{"2001:db8::/32", "123456789012", "eu-central-1"},
		})
		if len(table.Networks) != 0 {
			t.Fatalf("Networks = %d rows, want 0: %+v", len(table.Networks), table.Networks)
		}
		if len(table.Diagnostics) != 1 || table.Diagnostics[0].Level != LevelWarning {
			t.Fatalf("Diagnostics = %+v, want exactly one warning", table.Diagnostics)
		}
	})
	t.Run("ranges start/end columns", func(t *testing.T) {
		kind, columns := resolve(t, []string{"Start Address", "End Address"})
		table := Normalize(kind, columns, [][]string{
			{"2001:db8::1", "2001:db8::2"},
		})
		if len(table.Ranges) != 0 {
			t.Fatalf("Ranges = %d rows, want 0: %+v", len(table.Ranges), table.Ranges)
		}
		for _, l := range diagLevels(table.Diagnostics) {
			if l != LevelWarning {
				t.Errorf("diagnostic level = %q, want warning for every diagnostic: %+v", l, table.Diagnostics)
			}
		}
		if len(table.Diagnostics) == 0 {
			t.Error("Diagnostics is empty, want at least one warning")
		}
	})
}

// --- design section 4, row: empty rows and repeated header rows ---

func TestNormalizeSkipsEmptyAndRepeatedHeaderRows(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID", "Account Name"})
	table := Normalize(kind, columns, [][]string{
		{"", ""},                       // row 1: empty, skipped
		{"Account ID", "Account Name"}, // row 2: repeated header, skipped
		{"123456789012", "Example Co"}, // row 3: real data
	})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1: %+v", len(table.Accounts), table.Accounts)
	}
	if got := table.Accounts[0].SourceRow; got != 3 {
		t.Errorf("SourceRow = %d, want 3 (source row numbering counts skipped rows too)", got)
	}
	if got := table.Accounts[0].AccountName; got != "Example Co" {
		t.Errorf("AccountName = %q, want %q", got, "Example Co")
	}
	if len(table.Diagnostics) != 0 {
		t.Errorf("Diagnostics = %+v, want none (skips are silent)", table.Diagnostics)
	}
}

// --- header alias + unknown-column integration (see also headers_test.go) ---

// An unresolved required column must fail ResolveHeaders itself; Normalize
// is never reached in that case. Regression-covered here by exercising the
// full parse -> resolve -> normalize path a caller would actually take.
func TestNormalizeRequiredColumnMissingStopsBeforeNormalize(t *testing.T) {
	_, _, err := ResolveHeaders([]string{"Account Name", "Owner"})
	if err == nil {
		t.Fatal("ResolveHeaders: want an error for a table with no resolvable kind")
	}
}

// Unknown columns are never dropped: their value lands in the row's
// description, labeled with the original header text.
func TestNormalizeAppendsUnknownColumnsToDescription(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID", "Cost Center"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "CC-42"},
	})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(table.Accounts))
	}
	if want := "Cost Center: CC-42"; !strings.Contains(table.Accounts[0].Description, want) {
		t.Errorf("Description = %q, want it to contain %q", table.Accounts[0].Description, want)
	}
}

// regions is split on ';', ',' and whitespace (design section 4).
func TestNormalizeSplitsRegionsCell(t *testing.T) {
	kind, columns := resolve(t, []string{"Account ID", "Regions"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "eu-central-1; eu-west-1,us-east-1  ap-southeast-2"},
	})
	if len(table.Accounts) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(table.Accounts))
	}
	want := []string{"eu-central-1", "eu-west-1", "us-east-1", "ap-southeast-2"}
	got := table.Accounts[0].Regions
	if len(got) != len(want) {
		t.Fatalf("Regions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Regions[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Found by running `onboard parse` on a real networks CSV: the account-id
// repairs were wired into accounts tables only, so a networks table -- which
// is what the organization inventory produces -- passed damaged ids through.
func TestNormalizeRepairsAccountIDsInNetworksTables(t *testing.T) {
	kind, columns, err := ResolveHeaders([]string{"Account ID", "Region", "CIDR"})
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	table := Normalize(kind, columns, [][]string{
		{"12345678", "eu-central-1", "10.1.0.0/16"},
		{"1234-5678-9012", "eu-central-1", "10.2.0.0/16"},
		{"1.23457E+11", "eu-central-1", "10.3.0.0/16"},
	})
	if len(table.Networks) != 2 {
		t.Fatalf("rows = %d, want 2 (the scientific-notation row must be refused): %+v", len(table.Networks), table.Networks)
	}
	if got := table.Networks[0].AccountID; got != "000012345678" {
		t.Errorf("short id = %q, want left-padded 000012345678", got)
	}
	if got := table.Networks[1].AccountID; got != "123456789012" {
		t.Errorf("dashed id = %q, want separators removed", got)
	}
	var warned, refused bool
	for _, d := range table.Diagnostics {
		if d.Row == 1 && d.Level == LevelWarning {
			warned = true
		}
		if d.Row == 3 && d.Level == LevelError {
			refused = true
		}
	}
	if !warned {
		t.Errorf("no warning for the left-padded id: %+v", table.Diagnostics)
	}
	if !refused {
		t.Errorf("no error for the scientific-notation id: %+v", table.Diagnostics)
	}
}

// Found with a real workbook (package D2): a merged header cell leaves a
// column with no header, and its data used to vanish without a diagnostic.
func TestNormalizeKeepsDataUnderBlankHeaderAndBeyondHeaderRow(t *testing.T) {
	kind, columns, err := ResolveHeaders([]string{"Account ID", "Region", "CIDR", "", "Name"})
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	table := Normalize(kind, columns, [][]string{
		{"111111111111", "eu-central-1", "10.1.0.0/16", "merged-header note", "orders", "past the header row"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	description := table.Networks[0].Description
	for _, want := range []string{"column D: merged-header note", "column F: past the header row"} {
		if !strings.Contains(description, want) {
			t.Errorf("description %q does not keep %q", description, want)
		}
	}
	for index, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 701: "ZZ"} {
		if got := columnLetter(index); got != want {
			t.Errorf("columnLetter(%d) = %q, want %q", index, got, want)
		}
	}
}

// --- ADR 0014 / docs/WORK_PLAN.md M1b1: association_id, observed_at ---

func TestNormalizeCarriesAssociationIDAndObservedAt(t *testing.T) {
	kind, columns := resolve(t, []string{"account_id", "cidr", "region", "association_id", "observed_at"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "10.4.0.0/16", "eu-central-1", "vpc-cidr-assoc-0001", "2026-09-20T12:00:00Z"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	row := table.Networks[0]
	if row.AssociationID != "vpc-cidr-assoc-0001" {
		t.Errorf("AssociationID = %q, want vpc-cidr-assoc-0001", row.AssociationID)
	}
	if row.ObservedAt != "2026-09-20T12:00:00Z" {
		t.Errorf("ObservedAt = %q, want the raw cell value", row.ObservedAt)
	}
	wantParsed, err := time.Parse(time.RFC3339, "2026-09-20T12:00:00Z")
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if !row.ObservedAtParsed.Equal(wantParsed) {
		t.Errorf("ObservedAtParsed = %v, want %v", row.ObservedAtParsed, wantParsed)
	}
}

// An observed_at that does not parse as RFC 3339 is preserved verbatim in
// ObservedAt (ADR 0014: "a value that does not parse is NOT an error for the
// import"): ObservedAtParsed stays the zero Time, no Diagnostic is raised,
// and the row is not dropped -- the import never reads the parsed value, so
// there is nothing for a bad one to break; it is the assessment
// (internal/assess) that turns this into a reported limit.
func TestNormalizeObservedAtUnparsablePreservedVerbatimNoDiagnostic(t *testing.T) {
	kind, columns := resolve(t, []string{"account_id", "cidr", "region", "observed_at"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "10.4.1.0/24", "eu-central-1", "sometime last week"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	row := table.Networks[0]
	if row.ObservedAt != "sometime last week" {
		t.Errorf("ObservedAt = %q, want the raw unparsable value preserved verbatim", row.ObservedAt)
	}
	if !row.ObservedAtParsed.IsZero() {
		t.Errorf("ObservedAtParsed = %v, want the zero Time for an unparsable value", row.ObservedAtParsed)
	}
	if len(table.Diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want none: an unparsable observed_at is not an import error", table.Diagnostics)
	}
}

// An old input with neither column parses and normalizes exactly as before:
// both new fields stay at their zero value, and no diagnostic is raised for
// their absence.
func TestNormalizeWithoutNewColumnsLeavesThemZeroValue(t *testing.T) {
	kind, columns := resolve(t, []string{"account_id", "cidr", "region"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "10.4.2.0/24", "eu-central-1"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	row := table.Networks[0]
	if row.AssociationID != "" || row.ObservedAt != "" || !row.ObservedAtParsed.IsZero() || row.SourceFile != "" {
		t.Errorf("row = %+v, want every new field at its zero value", row)
	}
	if row.AssociationIDColumnPresent || row.ObservedAtColumnPresent {
		t.Errorf("row = %+v, want both *ColumnPresent flags false when the columns are absent", row)
	}
}

// --- docs/WORK_PLAN.md M1b3: column-presence flags ---
//
// The gap M1b1's review left for M1b3: AssociationID and ObservedAt alone
// cannot tell "no such column" from "column present, this row's cell blank",
// because both read back as "". These tests are the additive fix's own
// evidence, independent of package internal/assess or internal/onboardcmd.

// A subnet row legitimately has an empty AssociationID cell even when the
// association_id column exists (the collector never writes one for a subnet
// row) -- exactly the case AssociationIDColumnPresent exists to distinguish
// from "the column was never in this file at all".
func TestNormalizeColumnPresentButRowCellEmpty(t *testing.T) {
	kind, columns := resolve(t, []string{"account_id", "cidr", "region", "type", "association_id", "observed_at"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "10.4.3.0/24", "eu-central-1", "subnet", "", "2026-09-20T12:00:00Z"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	row := table.Networks[0]
	if row.AssociationID != "" {
		t.Errorf("AssociationID = %q, want empty (subnet row)", row.AssociationID)
	}
	if !row.AssociationIDColumnPresent {
		t.Error("AssociationIDColumnPresent = false, want true: the column was in the header, only this row's cell was blank")
	}
	if !row.ObservedAtColumnPresent {
		t.Error("ObservedAtColumnPresent = false, want true")
	}
}

// The two flags are independent: a table may carry one new column without
// the other.
func TestNormalizeColumnPresenceFlagsAreIndependent(t *testing.T) {
	kind, columns := resolve(t, []string{"account_id", "cidr", "region", "association_id"})
	table := Normalize(kind, columns, [][]string{
		{"123456789012", "10.4.4.0/24", "eu-central-1", "vpc-cidr-assoc-9"},
	})
	if len(table.Networks) != 1 {
		t.Fatalf("rows = %d, diagnostics = %+v", len(table.Networks), table.Diagnostics)
	}
	row := table.Networks[0]
	if !row.AssociationIDColumnPresent {
		t.Error("AssociationIDColumnPresent = false, want true")
	}
	if row.ObservedAtColumnPresent {
		t.Error("ObservedAtColumnPresent = true, want false: no observed_at column was in this header")
	}
}

// concatTables (internal/onboardcmd) merges rows from several inputs without
// touching their fields, so the presence flags must already be correct
// PER ROW at the point Normalize returns it -- this test constructs two
// separate Tables (one per simulated input, exactly as two separate ReadTable
// calls would) and checks each survives independently, standing in for "an
// old file without the new columns mixed with a new one" without needing
// internal/onboardcmd's concatTables in this package's own test.
func TestNormalizeColumnPresenceIsPerInputNotGlobal(t *testing.T) {
	oldKind, oldColumns := resolve(t, []string{"account_id", "cidr", "region"})
	oldTable := Normalize(oldKind, oldColumns, [][]string{
		{"123456789012", "10.4.5.0/24", "eu-central-1"},
	})
	newKind, newColumns := resolve(t, []string{"account_id", "cidr", "region", "association_id", "observed_at"})
	newTable := Normalize(newKind, newColumns, [][]string{
		{"234567890123", "10.4.6.0/24", "eu-central-1", "vpc-cidr-assoc-10", "2026-09-20T13:00:00Z"},
	})

	if len(oldTable.Networks) != 1 || len(newTable.Networks) != 1 {
		t.Fatalf("oldTable.Networks = %d, newTable.Networks = %d, want 1 and 1", len(oldTable.Networks), len(newTable.Networks))
	}
	oldRow, newRow := oldTable.Networks[0], newTable.Networks[0]
	if oldRow.AssociationIDColumnPresent || oldRow.ObservedAtColumnPresent {
		t.Errorf("old-format row = %+v, want both flags false", oldRow)
	}
	if !newRow.AssociationIDColumnPresent || !newRow.ObservedAtColumnPresent {
		t.Errorf("new-format row = %+v, want both flags true", newRow)
	}
}
