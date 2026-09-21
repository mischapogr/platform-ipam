package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTestdata opens a fixture under testdata/ and returns it along with its
// size, ready for ReadTable (which needs io.ReaderAt + size).
func openTestdata(t *testing.T, name string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("opening testdata/%s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat testdata/%s: %v", name, err)
	}
	return f, info.Size()
}

func diagMessages(t *testing.T, diags []Diagnostic) []string {
	t.Helper()
	msgs := make([]string, len(diags))
	for i, d := range diags {
		msgs[i] = string(d.Level) + ": " + d.Message
	}
	return msgs
}

// TestReadTable_CSV_BOM_CRLF_Quoting reads testdata/networks.csv: a
// UTF-8-BOM-prefixed, CRLF-terminated CSV with a quoted field holding an
// embedded comma. Nothing in read.go strips the BOM directly -- this proves
// that relying on Normalize/ResolveHeaders' existing cleanCell is enough.
func TestReadTable_CSV_BOM_CRLF_Quoting(t *testing.T) {
	f, size := openTestdata(t, "networks.csv")
	table, err := ReadTable("networks.csv", f, size, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	if len(table.Networks) != 2 {
		t.Fatalf("got %d network rows, want 2 (diagnostics: %v)", len(table.Networks), diagMessages(t, table.Diagnostics))
	}
	got := table.Networks[0]
	if got.AccountID != "123456789012" || got.CIDR != "10.1.0.0/16" || got.Region != "eu-central-1" {
		t.Fatalf("row 0 = %+v", got)
	}
	if got.Name != "prod, primary" {
		t.Fatalf("Name = %q, want the embedded comma preserved by RFC 4180 quoting: %q", got.Name, "prod, primary")
	}
	if table.Networks[1].CIDR != "10.1.1.0/24" {
		t.Fatalf("row 1 CIDR = %q, want 10.1.1.0/24", table.Networks[1].CIDR)
	}
}

// TestReadTable_SemicolonCSV reads a .csv file that is actually
// semicolon-delimited (a German Excel export), with German header aliases,
// proving the sniff-over-comma-default in detectDelimiter.
func TestReadTable_SemicolonCSV(t *testing.T) {
	f, size := openTestdata(t, "networks_semicolon.csv")
	table, err := ReadTable("networks_semicolon.csv", f, size, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	if len(table.Networks) != 2 {
		t.Fatalf("got %d network rows, want 2 (diagnostics: %v)", len(table.Networks), diagMessages(t, table.Diagnostics))
	}
	if table.Networks[0].AccountID != "123456789012" || table.Networks[0].CIDR != "10.2.0.0/16" {
		t.Fatalf("row 0 = %+v", table.Networks[0])
	}
	if table.Networks[0].Name != "prod-vpc" {
		t.Fatalf("Name (from German 'Name' header) = %q, want prod-vpc", table.Networks[0].Name)
	}
}

// TestReadTable_ConfluencePaste reads a .txt Confluence paste: forced-tab
// delimiter, two leading blank lines to skip, NBSPs around the account id,
// and a quoted cell holding two CIDRs split by an embedded newline (design
// section 4: "several CIDRs in one cell ... separated by newline").
func TestReadTable_ConfluencePaste(t *testing.T) {
	f, size := openTestdata(t, "confluence_paste.txt")
	table, err := ReadTable("confluence_paste.txt", f, size, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	if len(table.Networks) != 2 {
		t.Fatalf("got %d network rows, want 2 (one source row splitting into two CIDRs); diagnostics: %v",
			len(table.Networks), diagMessages(t, table.Diagnostics))
	}
	for _, row := range table.Networks {
		if row.AccountID != "123456789012" {
			t.Errorf("AccountID = %q, want NBSPs stripped to 123456789012", row.AccountID)
		}
		if row.SourceRow != table.Networks[0].SourceRow {
			t.Errorf("both split CIDRs should share SourceRow, got %d and %d", row.SourceRow, table.Networks[0].SourceRow)
		}
	}
	if table.Networks[0].CIDR != "10.3.0.0/16" || table.Networks[1].CIDR != "10.3.1.0/24" {
		t.Fatalf("CIDRs = %q, %q; want 10.3.0.0/16 and 10.3.1.0/24", table.Networks[0].CIDR, table.Networks[1].CIDR)
	}
}

// TestReadTable_ConfluencePasteCarriesAssociationIDAndObservedAt is the
// Confluence-paste reader's coverage of ADR 0014's two new columns
// (docs/WORK_PLAN.md M1b1: "xlsx and Confluence-paste inputs carry the new
// columns too when present"), on the same tab-separated, no-extension shape
// TestReadTable_ConfluencePaste already exercises.
func TestReadTable_ConfluencePasteCarriesAssociationIDAndObservedAt(t *testing.T) {
	input := "account_id\tcidr\tregion\tassociation_id\tobserved_at\n" +
		"123456789012\t10.9.0.0/16\teu-central-1\tvpc-cidr-assoc-paste1\t2026-09-20T09:00:00Z\n"
	table, err := ReadText("pasted", strings.NewReader(input), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if len(table.Networks) != 1 {
		t.Fatalf("got %d network rows, want 1; diagnostics: %v", len(table.Networks), diagMessages(t, table.Diagnostics))
	}
	row := table.Networks[0]
	if row.AssociationID != "vpc-cidr-assoc-paste1" {
		t.Errorf("AssociationID = %q, want vpc-cidr-assoc-paste1", row.AssociationID)
	}
	if row.ObservedAt != "2026-09-20T09:00:00Z" {
		t.Errorf("ObservedAt = %q, want 2026-09-20T09:00:00Z", row.ObservedAt)
	}
	if row.ObservedAtParsed.IsZero() {
		t.Error("ObservedAtParsed should have parsed a well-formed RFC 3339 value")
	}
}

// TestReadText_FromReader exercises the pure io.Reader entry point (the
// shape package C5 will use for stdin), with no file extension to key off,
// so delimiter detection falls through to sniffing.
func TestReadText_FromReader(t *testing.T) {
	input := "account_id,account_name,environment\n123456789012,Example Co,prod\n"
	table, err := ReadText("-", strings.NewReader(input), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if table.Kind != KindAccounts {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindAccounts)
	}
	if len(table.Accounts) != 1 || table.Accounts[0].AccountID != "123456789012" {
		t.Fatalf("Accounts = %+v", table.Accounts)
	}
}

// TestReadText_RangesEndToEnd covers a ranges table read end to end,
// including the dash-address-range form, without re-testing every
// normalization rule already covered by normalize_test.go.
func TestReadText_RangesEndToEnd(t *testing.T) {
	input := "cidr,description\n10.0.0.1 - 10.0.0.50,test pool\n10.4.0.0/16,extra block\n"
	table, err := ReadText("ranges.csv", strings.NewReader(input), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if table.Kind != KindRanges {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindRanges)
	}
	if len(table.Ranges) != 2 {
		t.Fatalf("got %d range rows, want 2; diagnostics: %v", len(table.Ranges), diagMessages(t, table.Diagnostics))
	}
	if table.Ranges[0].StartAddress != "10.0.0.1" || table.Ranges[0].EndAddress != "10.0.0.50" {
		t.Fatalf("range 0 = %+v", table.Ranges[0])
	}
	if table.Ranges[1].CIDR != "10.4.0.0/16" {
		t.Fatalf("range 1 = %+v", table.Ranges[1])
	}
}

func TestReadTable_UnsupportedFormats(t *testing.T) {
	for _, ext := range []string{".xls", ".ods", ".pdf"} {
		t.Run(ext, func(t *testing.T) {
			r := strings.NewReader("whatever")
			_, err := ReadTable("workbook"+ext, r, int64(r.Len()), ReadOptions{})
			if err == nil {
				t.Fatalf("ReadTable(%q): want an error, got nil", ext)
			}
			if !strings.Contains(err.Error(), "export") {
				t.Fatalf("error %q does not tell the operator to export to CSV", err)
			}
		})
	}
}

func TestReadTable_XLSXExtensionWithoutZipMagic(t *testing.T) {
	r := strings.NewReader("not actually a zip file")
	_, err := ReadTable("workbook.xlsx", r, int64(r.Len()), ReadOptions{})
	if err == nil {
		t.Fatal("ReadTable: want an error for an .xlsx that is not a zip archive")
	}
	if !strings.Contains(err.Error(), "zip signature") {
		t.Fatalf("error = %q, want it to mention the missing zip signature", err)
	}
}

func TestReadText_AllBlankInput(t *testing.T) {
	_, err := ReadText("empty.csv", strings.NewReader("\n\n  \n"), ReadOptions{})
	if err == nil {
		t.Fatal("ReadText: want an error for an input with no header row")
	}
	if !strings.Contains(err.Error(), "no header row") {
		t.Fatalf("error = %q, want it to mention the missing header row", err)
	}
}

func TestDetectDelimiter(t *testing.T) {
	tests := []struct {
		name       string
		ext        string
		headerLine string
		want       rune
	}{
		{"csv extension, comma header", ".csv", "account_id,cidr,region", ','},
		{"csv extension, semicolon header (German export)", ".csv", "AWS Konto;Netz;Region", ';'},
		{"tsv extension always tab", ".tsv", "a,b;c", '\t'},
		{"txt extension always tab", ".txt", "a,b;c", '\t'},
		{"no extension, tab wins sniff", "", "account_id\tcidr\tregion", '\t'},
		{"no extension, semicolon wins sniff", "", "account_id;cidr;region", ';'},
		{"no extension, comma wins sniff", "", "account_id,cidr,region", ','},
		{"no extension, no delimiters at all", "", "account_id", ','},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectDelimiter(tt.ext, tt.headerLine); got != tt.want {
				t.Errorf("detectDelimiter(%q, %q) = %q, want %q", tt.ext, tt.headerLine, got, tt.want)
			}
		})
	}
}
