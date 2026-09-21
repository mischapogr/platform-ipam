package onboard

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildXLSXBytes assembles a minimal .xlsx (zip) archive in memory from raw
// XML strings, so fixtures stay readable in the test instead of living as
// opaque committed binaries (per the C2 work package's instruction).
func buildXLSXBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}
	return buf.Bytes()
}

const (
	xlWorkbookNS  = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	xlRelsNS      = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	xlWorksheetRT = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"
)

func wrapWorksheet(rowsXML string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet ` + xlWorkbookNS + `><sheetData>` + rowsXML + `</sheetData></worksheet>`
}

func newReaderAt(data []byte) *bytes.Reader { return bytes.NewReader(data) }

// TestReadXLSX_SharedAndInlineStrings_SparseRow builds a single-sheet
// workbook covering: shared strings (plain and a two-run rich-text value,
// including a reused index), inline strings, a numeric cell returned as the
// literal text of <v>, and a sparse row (its B cell is entirely absent, so
// it must come back empty rather than shifting later columns left).
func TestReadXLSX_SharedAndInlineStrings_SparseRow(t *testing.T) {
	sharedStrings := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<si><t>account_id</t></si>` + // 0
		`<si><t>cidr</t></si>` + // 1
		`<si><t>region</t></si>` + // 2
		`<si><t>10.1.0.0/16</t></si>` + // 3
		`<si><t>eu-central-1</t></si>` + // 4
		`<si><r><t>prod-</t></r><r><t>vpc</t></r></si>` + // 5, rich text "prod-vpc"
		`<si><t>10.1.1.0/24</t></si>` + // 6
		`</sst>`

	rows := `` +
		`<row r="1">` +
		`<c r="A1" t="s"><v>0</v></c>` +
		`<c r="B1" t="s"><v>1</v></c>` +
		`<c r="C1" t="s"><v>2</v></c>` +
		`<c r="D1" t="inlineStr"><is><t>name</t></is></c>` +
		`</row>` +
		// Row 2: sparse -- C2 (region) is entirely missing from the XML,
		// so it must read back empty rather than pulling D2's value into
		// C2's slot.
		`<row r="2">` +
		`<c r="A2" t="n"><v>123456789012</v></c>` +
		`<c r="B2" t="s"><v>3</v></c>` +
		`<c r="D2" t="s"><v>5</v></c>` +
		`</row>` +
		`<row r="3">` +
		`<c r="A3" t="n"><v>234567890123</v></c>` +
		`<c r="B3" t="s"><v>6</v></c>` +
		`<c r="C3" t="s"><v>4</v></c>` +
		`<c r="D3" t="inlineStr"><is><t>subnet-a</t></is></c>` +
		`</row>`

	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook ` + xlWorkbookNS + `><sheets>` +
			`<sheet name="Sheet1" sheetId="1" r:id="rId1"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships ` + xlRelsNS + `>` +
			`<Relationship Id="rId1" Type="` + xlWorksheetRT + `" Target="worksheets/sheet1.xml"/>` +
			`</Relationships>`,
		"xl/sharedStrings.xml":     sharedStrings,
		"xl/worksheets/sheet1.xml": wrapWorksheet(rows),
	}
	data := buildXLSXBytes(t, files)

	table, err := ReadTable("networks.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if table.Kind != KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindNetworks)
	}
	if len(table.Networks) != 2 {
		t.Fatalf("got %d network rows, want 2; diagnostics: %v", len(table.Networks), diagMessages(t, table.Diagnostics))
	}

	row2 := table.Networks[0]
	if row2.AccountID != "123456789012" || row2.CIDR != "10.1.0.0/16" {
		t.Fatalf("row 2 = %+v", row2)
	}
	if row2.Region != "" {
		t.Fatalf("Region = %q, want empty: C2 was never present in the sheet XML (sparse row)", row2.Region)
	}
	if row2.Name != "prod-vpc" {
		t.Fatalf("Name = %q, want the two rich-text runs concatenated into prod-vpc", row2.Name)
	}

	row3 := table.Networks[1]
	if row3.AccountID != "234567890123" || row3.CIDR != "10.1.1.0/24" || row3.Region != "eu-central-1" || row3.Name != "subnet-a" {
		t.Fatalf("row 3 = %+v", row3)
	}
}

// TestReadXLSX_SecondSheetByName_NumericAccountID builds a two-sheet
// workbook and selects the second by name, proving both opts.Sheet
// selection and that a numeric cell's <v> text reaches Normalize
// untouched: an 8-digit account id triggers C1's left-pad-with-warning
// repair, and Excel scientific notation triggers its error, exactly as
// they would from a typed decimal cell -- because our reader never
// interprets or reformats the number itself.
func TestReadXLSX_SecondSheetByName_NumericAccountID(t *testing.T) {
	// Sheet1 ("Data") deliberately has an unresolvable single header column,
	// so if the reader picked it instead of "Accounts" the test fails
	// loudly on ResolveHeaders rather than silently reading the wrong sheet.
	sheet1Rows := `<row r="1"><c r="A1" t="inlineStr"><is><t>notes</t></is></c></row>`

	sheet2Rows := `` +
		`<row r="1">` +
		`<c r="A1" t="inlineStr"><is><t>account_id</t></is></c>` +
		`<c r="B1" t="inlineStr"><is><t>account_name</t></is></c>` +
		`</row>` +
		// 8 digits: repaired by left-padding to 12, with a warning.
		`<row r="2">` +
		`<c r="A2" t="n"><v>12345678</v></c>` +
		`<c r="B2" t="inlineStr"><is><t>Example Co</t></is></c>` +
		`</row>` +
		// Excel scientific notation: the digits are already gone, so this
		// must be a hard error, and the row must not appear in Accounts.
		`<row r="3">` +
		`<c r="A3" t="n"><v>1.23457E+11</v></c>` +
		`<c r="B3" t="inlineStr"><is><t>Other Co</t></is></c>` +
		`</row>`

	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook ` + xlWorkbookNS + `><sheets>` +
			`<sheet name="Data" sheetId="1" r:id="rId1"/>` +
			`<sheet name="Accounts" sheetId="2" r:id="rId2"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships ` + xlRelsNS + `>` +
			`<Relationship Id="rId1" Type="` + xlWorksheetRT + `" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="` + xlWorksheetRT + `" Target="worksheets/sheet2.xml"/>` +
			`</Relationships>`,
		"xl/worksheets/sheet1.xml": wrapWorksheet(sheet1Rows),
		"xl/worksheets/sheet2.xml": wrapWorksheet(sheet2Rows),
	}
	data := buildXLSXBytes(t, files)

	table, err := ReadTable("accounts.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{Sheet: "Accounts"})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if table.Kind != KindAccounts {
		t.Fatalf("Kind = %q, want %q", table.Kind, KindAccounts)
	}
	if len(table.Accounts) != 1 {
		t.Fatalf("got %d account rows, want 1 (the scientific-notation row must be dropped); diagnostics: %v",
			len(table.Accounts), diagMessages(t, table.Diagnostics))
	}
	if table.Accounts[0].AccountID != "000012345678" {
		t.Fatalf("AccountID = %q, want left-padded to 12 digits", table.Accounts[0].AccountID)
	}

	foundWarning, foundError := false, false
	for _, d := range table.Diagnostics {
		switch {
		case d.Level == LevelWarning && strings.Contains(d.Message, "fewer than 12 digits"):
			foundWarning = true
		case d.Level == LevelError && strings.Contains(d.Message, "scientific notation"):
			foundError = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a left-pad warning diagnostic; got %v", diagMessages(t, table.Diagnostics))
	}
	if !foundError {
		t.Errorf("expected a scientific-notation error diagnostic; got %v", diagMessages(t, table.Diagnostics))
	}
}

// TestReadXLSX_CarriesAssociationIDAndObservedAt is the .xlsx reader's
// coverage of ADR 0014's two new columns (docs/WORK_PLAN.md M1b1: "xlsx and
// Confluence-paste inputs carry the new columns too when present"), using
// only inline strings so the fixture stays as small as the other xlsx_test.go
// cases that do not need shared-string coverage.
func TestReadXLSX_CarriesAssociationIDAndObservedAt(t *testing.T) {
	inline := func(v string) string { return `<c t="inlineStr"><is><t>` + v + `</t></is></c>` }
	rows := `<row r="1">` +
		inline("account_id") + inline("cidr") + inline("region") +
		inline("association_id") + inline("observed_at") +
		`</row>` +
		`<row r="2">` +
		inline("123456789012") + inline("10.9.5.0/24") + inline("eu-central-1") +
		inline("vpc-cidr-assoc-xlsx1") + inline("2026-09-20T10:00:00Z") +
		`</row>`

	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook ` + xlWorkbookNS + `><sheets>` +
			`<sheet name="Sheet1" sheetId="1" r:id="rId1"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships ` + xlRelsNS + `>` +
			`<Relationship Id="rId1" Type="` + xlWorksheetRT + `" Target="worksheets/sheet1.xml"/>` +
			`</Relationships>`,
		"xl/worksheets/sheet1.xml": wrapWorksheet(rows),
	}
	data := buildXLSXBytes(t, files)

	table, err := ReadTable("networks.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTable: %v", err)
	}
	if len(table.Networks) != 1 {
		t.Fatalf("got %d network rows, want 1; diagnostics: %v", len(table.Networks), diagMessages(t, table.Diagnostics))
	}
	row := table.Networks[0]
	if row.AssociationID != "vpc-cidr-assoc-xlsx1" {
		t.Errorf("AssociationID = %q, want vpc-cidr-assoc-xlsx1", row.AssociationID)
	}
	if row.ObservedAt != "2026-09-20T10:00:00Z" {
		t.Errorf("ObservedAt = %q, want 2026-09-20T10:00:00Z", row.ObservedAt)
	}
	if row.ObservedAtParsed.IsZero() {
		t.Error("ObservedAtParsed should have parsed a well-formed RFC 3339 value")
	}
	if row.SourceRow != 2 {
		t.Errorf("SourceRow = %d, want 2 (this package's own readers never set SourceFile)", row.SourceRow)
	}
}

func TestReadXLSX_SheetNotFound(t *testing.T) {
	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook ` + xlWorkbookNS + `><sheets>` +
			`<sheet name="Sheet1" sheetId="1" r:id="rId1"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships ` + xlRelsNS + `>` +
			`<Relationship Id="rId1" Type="` + xlWorksheetRT + `" Target="worksheets/sheet1.xml"/>` +
			`</Relationships>`,
		"xl/worksheets/sheet1.xml": wrapWorksheet(`<row r="1"><c r="A1" t="inlineStr"><is><t>account_id</t></is></c></row>`),
	}
	data := buildXLSXBytes(t, files)

	_, err := ReadTable("accounts.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{Sheet: "DoesNotExist"})
	if err == nil {
		t.Fatal("ReadTable: want an error for an unknown sheet name")
	}
	if !strings.Contains(err.Error(), "DoesNotExist") || !strings.Contains(err.Error(), "Sheet1") {
		t.Fatalf("error = %q, want it to name the requested and available sheets", err)
	}
}

func TestReadXLSX_NoSheets(t *testing.T) {
	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook ` + xlWorkbookNS + `><sheets></sheets></workbook>`,
	}
	data := buildXLSXBytes(t, files)

	_, err := ReadTable("empty.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{})
	if err == nil {
		t.Fatal("ReadTable: want an error for a workbook with no sheets")
	}
	if !strings.Contains(err.Error(), "no sheets") {
		t.Fatalf("error = %q, want it to mention the workbook has no sheets", err)
	}
}

// TestReadXLSX_ZipBombGuard builds an archive whose xl/workbook.xml
// decompresses past maxZipPartSize (a trivially compressible run of
// spaces makes this cheap to build and to reject) and asserts readXLSX
// refuses it via the declared-size check rather than inflating it.
func TestReadXLSX_ZipBombGuard(t *testing.T) {
	oversized := "<!--" + strings.Repeat(" ", maxZipPartSize+1) + "-->"
	files := map[string]string{
		"xl/workbook.xml": oversized,
	}
	data := buildXLSXBytes(t, files)

	_, err := ReadTable("bomb.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{})
	if err == nil {
		t.Fatal("ReadTable: want an error for a part over the per-part size cap")
	}
	if !strings.Contains(err.Error(), "per-part limit") {
		t.Fatalf("error = %q, want it to mention the per-part limit", err)
	}
}

func TestReadXLSX_MalformedXML(t *testing.T) {
	files := map[string]string{
		"xl/workbook.xml": "<workbook><sheets>not valid xml",
	}
	data := buildXLSXBytes(t, files)

	_, err := ReadTable("broken.xlsx", newReaderAt(data), int64(len(data)), ReadOptions{})
	if err == nil {
		t.Fatal("ReadTable: want an error for malformed xl/workbook.xml")
	}
}
