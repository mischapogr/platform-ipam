package onboard

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This file is package D2's verification pass: every fixture under
// testdata/real/ was written by a real, independent tool (never by this
// repository's code) -- see testdata/real/README.md for exactly which tool
// and version wrote which file, and testdata/real/gen/ for the generators.
// Unlike xlsx_test.go's hand-built-XML fixtures, these tests need no Docker
// and no Python: they only read the committed .xlsx files.

// openReal opens a fixture under testdata/real/ and returns it with its
// size, ready for ReadTable.
func openReal(t *testing.T, name string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "real", name))
	if err != nil {
		t.Fatalf("opening testdata/real/%s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat testdata/real/%s: %v", name, err)
	}
	return f, info.Size()
}

func readReal(t *testing.T, name string, opts ReadOptions) Table {
	t.Helper()
	f, size := openReal(t, name)
	table, err := ReadTable(name, f, size, opts)
	if err != nil {
		t.Fatalf("ReadTable(%s): %v", name, err)
	}
	return table
}

// wantNetworks is the Networks table every "main" fixture (openpyxl_main.xlsx
// and xlsxwriter_main.xlsx) must produce identically: both writers are given
// the same logical NETWORKS table (header Account ID, Region, CIDR, VPC ID,
// Name), so their real, independently-produced XML must normalize to the
// exact same rows. SourceRow is the true spreadsheet row: rows 1-7 are
// written, row 8 is left entirely untouched by both writers (a real
// spreadsheet tool omits an untouched row from the XML rather than emitting
// an empty <row> element), and row 9 follows the gap -- proving the reader
// numbers by the worksheet's own <row r="..."> attribute, not by position in
// the XML (package D2's main finding; see xlsx.go's remapXLSXRowNumbers).
func wantMainNetworks() []NetworkRow {
	return []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1", ResourceID: "vpc-0123abcd0000", Name: "prod-main"},
		{SourceRow: 3, CIDR: "10.1.0.0/16", AccountID: "000012345678", Region: "us-west-2", ResourceID: "vpc-0456def00000", Name: "staging"},
		{SourceRow: 4, CIDR: "10.2.0.0/16", AccountID: "000123456789", Region: "eu-west-1", ResourceID: "vpc-0789ghi00000", Name: "dev"},
		{SourceRow: 5, CIDR: "10.3.0.0/16", AccountID: "123456789012", Region: "ap-southeast-1", ResourceID: "vpc-0abc12340000", Name: "qa"},
		{SourceRow: 6, CIDR: "10.4.0.0/16", AccountID: "234567890123", Region: "sa-east-1", ResourceID: "vpc-0def56780000", Name: "multi-cidr"},
		{SourceRow: 6, CIDR: "10.5.0.0/16", AccountID: "234567890123", Region: "sa-east-1", ResourceID: "vpc-0def56780000", Name: "multi-cidr"},
		{SourceRow: 7, CIDR: "10.6.0.0/16", AccountID: "345678901234", Region: "ca-central-1", ResourceID: "vpc-0ghi90120000", Name: "Acme, Inc"},
		// row 8 never existed in the sheet XML (see fixture generators).
		{SourceRow: 9, CIDR: "10.7.0.0/16", AccountID: "456789012345", Region: "af-south-1", ResourceID: "vpc-0jkl34560000", Name: "after-gap"},
	}
}

func wantMainAccountIDWarning(row int, orig, padded string) Diagnostic {
	return Diagnostic{Level: LevelWarning, Row: row, Column: colAccountID,
		Message: `account id "` + orig + `" has fewer than 12 digits; left-padded to "` + padded + `"`}
}

// assertNetworks compares got against want field-by-field so a mismatch
// names which row and field, rather than dumping two whole slices.
func assertNetworks(t *testing.T, fixture string, got, want []NetworkRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d network rows, want %d\n got:  %+v\n want: %+v", fixture, len(got), len(want), got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s: Networks[%d] =\n got:  %+v\n want: %+v", fixture, i, got[i], want[i])
		}
	}
}

func assertAccounts(t *testing.T, fixture string, got, want []AccountRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d account rows, want %d\n got:  %+v\n want: %+v", fixture, len(got), len(want), got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("%s: Accounts[%d] =\n got:  %+v\n want: %+v", fixture, i, got[i], want[i])
		}
	}
}

func assertDiagnostics(t *testing.T, fixture string, got, want []Diagnostic) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: Diagnostics =\n got:  %+v\n want: %+v", fixture, got, want)
	}
}

// --- openpyxl (openpyxl 3.1.5; see testdata/real/README.md) ---------------

// TestReadXLSXReal_OpenpyxlMain reads a workbook written entirely by
// openpyxl: no hand-built XML anywhere. It covers, in one file: a normal
// row; an account id stored as a NUMBER that lost its leading zeros; an
// account id stored as TEXT with leading zeros preserved; a 12-digit account
// id stored as a NUMBER (openpyxl writes it as plain digits, "123456789012"
// -- never scientific notation, so it must NOT trigger the scientific-
// notation error); a CIDR cell holding two CIDRs separated by a line break;
// a name with a comma and a non-breaking space; and a blank row that
// openpyxl omitted entirely from the sheet XML.
func TestReadXLSXReal_OpenpyxlMain(t *testing.T) {
	table := readReal(t, "openpyxl_main.xlsx", ReadOptions{})
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	assertNetworks(t, "openpyxl_main.xlsx", table.Networks, wantMainNetworks())
	assertDiagnostics(t, "openpyxl_main.xlsx", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(3, "12345678", "000012345678"),
	})
}

// TestReadXLSXReal_OpenpyxlMainAccountsSheet selects the second sheet
// ("Accounts") by name via ReadOptions.Sheet, in the same real workbook.
func TestReadXLSXReal_OpenpyxlMainAccountsSheet(t *testing.T) {
	table := readReal(t, "openpyxl_main.xlsx", ReadOptions{Sheet: "Accounts"})
	if table.Kind != KindAccounts {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindAccounts)
	}
	assertAccounts(t, "openpyxl_main.xlsx#Accounts", table.Accounts, []AccountRow{
		{SourceRow: 2, AccountID: "111111111111", AccountName: "Example Co"},
		{SourceRow: 3, AccountID: "000022222222", AccountName: "Other Co"},
	})
	assertDiagnostics(t, "openpyxl_main.xlsx#Accounts", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(3, "22222222", "000022222222"),
	})
}

// TestReadXLSXReal_OpenpyxlMergedHeader: header cells D1:E1 are merged with
// the value "VPC ID / Name" -- real OOXML holds a value only in a merge's
// top-left cell, so E1 (the visual "second half" of the merge) does not
// exist as a cell at all. That leaves column E with a blank header. This
// fixture is what showed that such a column's data used to vanish without a
// diagnostic; it is now kept under its spreadsheet letter ("column E: ..."),
// for text formats as well.
func TestReadXLSXReal_OpenpyxlMergedHeader(t *testing.T) {
	table := readReal(t, "openpyxl_merged_header.xlsx", ReadOptions{})
	assertNetworks(t, "openpyxl_merged_header.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1",
			Description: "VPC ID / Name: vpc-0123abcd0000; column E: prod-main"},
	})
	assertDiagnostics(t, "openpyxl_merged_header.xlsx", table.Diagnostics, nil)
}

// TestReadXLSXReal_OpenpyxlStyledDate: the account id cell carries bold
// font + fill styling, and an extra "Onboarded" column holds a real
// date-formatted cell (Excel serial 45306 = 2024-01-15, number format
// MM/DD/YYYY). Neither the style nor the date format is evaluated -- exactly
// per docs/ONBOARDING_IMPORT.md section 3 ("cached cell values only");
// the serial number passes through as the literal <v> text, same as any
// other unknown column.
func TestReadXLSXReal_OpenpyxlStyledDate(t *testing.T) {
	table := readReal(t, "openpyxl_styled_date.xlsx", ReadOptions{})
	assertNetworks(t, "openpyxl_styled_date.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1",
			ResourceID: "vpc-0123abcd0000", Name: "prod-main", Description: "Onboarded: 45306"},
	})
	assertDiagnostics(t, "openpyxl_styled_date.xlsx", table.Diagnostics, nil)
}

// TestReadXLSXReal_OpenpyxlHeaderOffset: the header row is genuinely not
// row 1 -- spreadsheet rows 1 and 2 were never touched by openpyxl at all,
// so they are entirely absent from the worksheet XML, and the real header
// sits at row 3. Every data SourceRow must be the true spreadsheet row
// (4..10), not the header's XML *position* (which is 0, the first <row>
// element present) plus a naive +1.
func TestReadXLSXReal_OpenpyxlHeaderOffset(t *testing.T) {
	table := readReal(t, "openpyxl_header_offset.xlsx", ReadOptions{})
	want := []NetworkRow{
		{SourceRow: 4, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1", ResourceID: "vpc-0123abcd0000", Name: "prod-main"},
		{SourceRow: 5, CIDR: "10.1.0.0/16", AccountID: "000012345678", Region: "us-west-2", ResourceID: "vpc-0456def00000", Name: "staging"},
		{SourceRow: 6, CIDR: "10.2.0.0/16", AccountID: "000123456789", Region: "eu-west-1", ResourceID: "vpc-0789ghi00000", Name: "dev"},
		{SourceRow: 7, CIDR: "10.3.0.0/16", AccountID: "123456789012", Region: "ap-southeast-1", ResourceID: "vpc-0abc12340000", Name: "qa"},
		{SourceRow: 8, CIDR: "10.4.0.0/16", AccountID: "234567890123", Region: "sa-east-1", ResourceID: "vpc-0def56780000", Name: "multi-cidr"},
		{SourceRow: 8, CIDR: "10.5.0.0/16", AccountID: "234567890123", Region: "sa-east-1", ResourceID: "vpc-0def56780000", Name: "multi-cidr"},
		{SourceRow: 9, CIDR: "10.6.0.0/16", AccountID: "345678901234", Region: "ca-central-1", ResourceID: "vpc-0ghi90120000", Name: "Acme, Inc"},
		{SourceRow: 10, CIDR: "10.7.0.0/16", AccountID: "456789012345", Region: "af-south-1", ResourceID: "vpc-0jkl34560000", Name: "after-gap"},
	}
	assertNetworks(t, "openpyxl_header_offset.xlsx", table.Networks, want)
	assertDiagnostics(t, "openpyxl_header_offset.xlsx", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(5, "12345678", "000012345678"),
	})
}

// --- XlsxWriter (xlsxwriter 3.2.9; see testdata/real/README.md) -----------

// TestReadXLSXReal_XlsxwriterMain is XlsxWriter's independent construction
// of the exact same logical table as openpyxl_main.xlsx (XlsxWriter uses
// shared strings throughout; openpyxl used inline strings for this file --
// see README.md -- so the two fixtures also cross-check sstXML handling).
// It must normalize identically to wantMainNetworks(), proving the reader's
// behavior is not an accident of one writer's XML shape.
func TestReadXLSXReal_XlsxwriterMain(t *testing.T) {
	table := readReal(t, "xlsxwriter_main.xlsx", ReadOptions{})
	assertNetworks(t, "xlsxwriter_main.xlsx", table.Networks, wantMainNetworks())
	assertDiagnostics(t, "xlsxwriter_main.xlsx", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(3, "12345678", "000012345678"),
	})
}

func TestReadXLSXReal_XlsxwriterMainAccountsSheet(t *testing.T) {
	table := readReal(t, "xlsxwriter_main.xlsx", ReadOptions{Sheet: "Accounts"})
	assertAccounts(t, "xlsxwriter_main.xlsx#Accounts", table.Accounts, []AccountRow{
		{SourceRow: 2, AccountID: "111111111111", AccountName: "Example Co"},
		{SourceRow: 3, AccountID: "000022222222", AccountName: "Other Co"},
	})
	assertDiagnostics(t, "xlsxwriter_main.xlsx#Accounts", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(3, "22222222", "000022222222"),
	})
}

// TestReadXLSXReal_XlsxwriterFormulaCached: A1's account id comes from
// =TEXT(123456789012,"0") with an explicit cached value, written to OOXML
// as <c t="str"><f>...</f><v>123456789012</v></c> -- exactly the shape
// xlsx.go's cellValue comment describes for a "str" formula result. F2 is a
// numeric-result formula (=1+1, cached 2, no t attribute) in an extra
// column, proving a plain numeric <f>+<v> cell is also passed through.
func TestReadXLSXReal_XlsxwriterFormulaCached(t *testing.T) {
	table := readReal(t, "xlsxwriter_formula_cached.xlsx", ReadOptions{})
	assertNetworks(t, "xlsxwriter_formula_cached.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1",
			ResourceID: "vpc-0123abcd0000", Name: "prod-main", Description: "block_count: 2"},
	})
	assertDiagnostics(t, "xlsxwriter_formula_cached.xlsx", table.Diagnostics, nil)
}

// TestReadXLSXReal_XlsxwriterEmptyColumn: column D sits entirely empty
// between CIDR (C) and VPC ID (E) -- header and every data row. Because
// every surviving cell in the real XML is placed by its own reference
// (parseCellRef), not by position, the gap costs nothing: VPC ID and Name
// still land in the right fields.
func TestReadXLSXReal_XlsxwriterEmptyColumn(t *testing.T) {
	table := readReal(t, "xlsxwriter_empty_column.xlsx", ReadOptions{})
	assertNetworks(t, "xlsxwriter_empty_column.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1", ResourceID: "vpc-0123abcd0000", Name: "prod-main"},
		{SourceRow: 3, CIDR: "10.1.0.0/16", AccountID: "234567890123", Region: "us-west-2", ResourceID: "vpc-0456def00000", Name: "staging"},
	})
	assertDiagnostics(t, "xlsxwriter_empty_column.xlsx", table.Diagnostics, nil)
}

// TestReadXLSXReal_XlsxwriterFrozenAutofilter: a frozen pane (<pane> inside
// <sheetView>) and an autofilter (<autoFilter>) are metadata the sheetXML
// struct never maps, so encoding/xml silently ignores them, exactly as
// docs/ONBOARDING_IMPORT.md's "the reader must ignore" requires.
func TestReadXLSXReal_XlsxwriterFrozenAutofilter(t *testing.T) {
	table := readReal(t, "xlsxwriter_frozen_autofilter.xlsx", ReadOptions{})
	assertNetworks(t, "xlsxwriter_frozen_autofilter.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1", ResourceID: "vpc-0123abcd0000", Name: "prod-main"},
		{SourceRow: 3, CIDR: "10.1.0.0/16", AccountID: "234567890123", Region: "us-west-2", ResourceID: "vpc-0456def00000", Name: "staging"},
	})
	assertDiagnostics(t, "xlsxwriter_frozen_autofilter.xlsx", table.Diagnostics, nil)
}

// TestReadXLSXReal_XlsxwriterWideColumn: "Name" sits in column ZZ (0-based
// index 701), well past parseCellRef's single- and double-letter fixtures
// elsewhere in this package, proving its base-26 decoding is correct there
// too.
func TestReadXLSXReal_XlsxwriterWideColumn(t *testing.T) {
	table := readReal(t, "xlsxwriter_wide_column.xlsx", ReadOptions{})
	assertNetworks(t, "xlsxwriter_wide_column.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.0.0.0/16", AccountID: "123456789012", Region: "us-east-1", Name: "prod-main"},
	})
	assertDiagnostics(t, "xlsxwriter_wide_column.xlsx", table.Diagnostics, nil)
}

// --- LibreOffice (headless CSV -> xlsx conversion; see README.md) ---------

// TestReadXLSXReal_LibreOfficeFromCSV reads a workbook LibreOffice itself
// produced by converting a CSV -- "soffice --headless --convert-to xlsx" --
// which is the form an operator most plausibly hands over. LibreOffice's
// own CSV importer applies automatic type detection to the account_id
// column and silently turns "012345678901" and "098765432109" into NUMBERS,
// dropping their leading zero, exactly the real-world damage
// docs/ONBOARDING_IMPORT.md section 4 describes -- reproduced here by a
// genuinely independent tool rather than asserted by hand. Both are
// well under 12 digits after the loss, so both are repaired with a warning,
// and by coincidence of only ever losing a single leading zero, the
// left-padded repair reconstructs the original text exactly.
func TestReadXLSXReal_LibreOfficeFromCSV(t *testing.T) {
	table := readReal(t, "libreoffice_from_csv.xlsx", ReadOptions{})
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	assertNetworks(t, "libreoffice_from_csv.xlsx", table.Networks, []NetworkRow{
		{SourceRow: 2, CIDR: "10.10.0.0/16", AccountID: "123456789012", Region: "eu-central-1", Name: "prod, primary"},
		{SourceRow: 3, CIDR: "10.11.0.0/16", AccountID: "012345678901", Region: "eu-central-1", Name: "dev-account"},
		{SourceRow: 4, CIDR: "10.12.0.0/16", AccountID: "098765432109", Region: "us-east-1", Name: "legacy VPC"},
	})
	assertDiagnostics(t, "libreoffice_from_csv.xlsx", table.Diagnostics, []Diagnostic{
		wantMainAccountIDWarning(3, "12345678901", "012345678901"),
		wantMainAccountIDWarning(4, "98765432109", "098765432109"),
	})
}
