package onboard

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// maxZipPartSize caps the uncompressed size of any single part read out of
// an .xlsx archive (design: "guard against zip bombs ... cap uncompressed
// size per part, e.g. 64 MiB"). An onboarding spreadsheet is at most a few
// thousand rows; 64 MiB is generous headroom while still refusing a crafted
// archive that decompresses far past what any real workbook needs.
const maxZipPartSize = 64 << 20

// wbXML is the subset of xl/workbook.xml this reader needs: the ordered
// list of sheets, each with its display name and the relationship id that
// xl/_rels/workbook.xml.rels resolves to a worksheet part. Go's encoding/xml
// matches struct tags without a namespace by local name only, so "id,attr"
// matches the namespace-prefixed r:id attribute regardless of which prefix
// the producer bound to that namespace.
type wbXML struct {
	Sheets []wbSheetXML `xml:"sheets>sheet"`
}

type wbSheetXML struct {
	Name string `xml:"name,attr"`
	RID  string `xml:"id,attr"`
}

// relsXML is xl/_rels/workbook.xml.rels: relationship id -> part target.
type relsXML struct {
	Relationships []relXML `xml:"Relationship"`
}

type relXML struct {
	ID     string `xml:"Id,attr"`
	Target string `xml:"Target,attr"`
}

// sstXML is xl/sharedStrings.xml. Each <si> is either a plain <t> (T is
// set, R is empty) or one or more rich-text runs <r><t>...</t></r> (T is
// empty, R holds each run's text in order); Go's encoding/xml maps "t"
// (unqualified path) to only the *direct* child element, so a plain <si>'s
// <t> never gets confused with a rich <si>'s nested <r><t>.
type sstXML struct {
	SI []siXML `xml:"si"`
}

type siXML struct {
	T string   `xml:"t"`
	R []runXML `xml:"r"`
}

type runXML struct {
	T string `xml:"t"`
}

// sheetXML is one worksheet part: xl/worksheets/sheetN.xml.
type sheetXML struct {
	Rows []rowXML `xml:"sheetData>row"`
}

type rowXML struct {
	R     string    `xml:"r,attr"`
	Cells []cellXML `xml:"c"`
}

type cellXML struct {
	R  string        `xml:"r,attr"`
	T  string        `xml:"t,attr"`
	V  string        `xml:"v"`
	Is *inlineStrXML `xml:"is"`
}

type inlineStrXML struct {
	T string   `xml:"t"`
	R []runXML `xml:"r"`
}

// readXLSX implements the .xlsx branch of ReadTable: resolve the target
// sheet through xl/workbook.xml + xl/_rels/workbook.xml.rels, decode shared
// strings, parse the worksheet's cells by reference (not position, so a
// sparse row places its values correctly), and hand the first row off as
// the header to ResolveHeaders/Normalize exactly like a text table.
func readXLSX(name string, ra io.ReaderAt, size int64, opts ReadOptions) (Table, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return Table{}, fmt.Errorf("%s: not a valid xlsx (zip) archive: %w", name, err)
	}

	wbData, err := readZipPart(zr, "xl/workbook.xml")
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}
	var wb wbXML
	if err := xml.Unmarshal(wbData, &wb); err != nil {
		return Table{}, fmt.Errorf("%s: parsing xl/workbook.xml: %w", name, err)
	}
	if len(wb.Sheets) == 0 {
		return Table{}, fmt.Errorf("%s: workbook has no sheets", name)
	}

	sheet := wb.Sheets[0]
	if opts.Sheet != "" {
		found := false
		for _, s := range wb.Sheets {
			if s.Name == opts.Sheet {
				sheet = s
				found = true
				break
			}
		}
		if !found {
			names := make([]string, len(wb.Sheets))
			for i, s := range wb.Sheets {
				names[i] = s.Name
			}
			return Table{}, fmt.Errorf("%s: sheet %q not found; sheets in this workbook: %s",
				name, opts.Sheet, strings.Join(names, ", "))
		}
	}

	relsData, err := readZipPart(zr, "xl/_rels/workbook.xml.rels")
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}
	var rels relsXML
	if err := xml.Unmarshal(relsData, &rels); err != nil {
		return Table{}, fmt.Errorf("%s: parsing xl/_rels/workbook.xml.rels: %w", name, err)
	}
	relTargets := make(map[string]string, len(rels.Relationships))
	for _, r := range rels.Relationships {
		relTargets[r.ID] = r.Target
	}
	sheetPath, err := resolveSheetPath(sheet, relTargets)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}

	var sst []string
	if sstData, err := readZipPart(zr, "xl/sharedStrings.xml"); err == nil {
		var s sstXML
		if err := xml.Unmarshal(sstData, &s); err != nil {
			return Table{}, fmt.Errorf("%s: parsing xl/sharedStrings.xml: %w", name, err)
		}
		sst = make([]string, len(s.SI))
		for i, si := range s.SI {
			sst[i] = joinRuns(si.T, si.R)
		}
	}
	// A workbook with no string cells at all may omit sharedStrings.xml
	// entirely; sst stays nil, and any cell that then claims type "s" fails
	// in cellValue below with a clear out-of-range error rather than a
	// silent empty value.

	sheetData, err := readZipPart(zr, sheetPath)
	if err != nil {
		return Table{}, fmt.Errorf("%s: reading sheet %q: %w", name, sheet.Name, err)
	}
	var sx sheetXML
	if err := xml.Unmarshal(sheetData, &sx); err != nil {
		return Table{}, fmt.Errorf("%s: parsing sheet %q: %w", name, sheet.Name, err)
	}

	grid, rowNums, err := rowsToGrid(sheet.Name, sx.Rows, sst)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}

	idx := firstNonBlankGridRow(grid)
	if idx < 0 {
		return Table{}, fmt.Errorf("%s: sheet %q has no header row (sheet is empty)", name, sheet.Name)
	}
	headers := grid[idx]
	dataRows := grid[idx+1:]
	dataRowNums := rowNums[idx+1:]

	kind, columns, err := ResolveHeaders(headers)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}
	// Real worksheets, unlike a text table, can omit an entirely blank row
	// from the XML altogether rather than emitting a row with no cells (both
	// LibreOffice and openpyxl do this; verified against real files, package
	// D2). A constant offset (what shiftRows applies for CSV/TSV, where every
	// physical line is always present) would then under-count every row
	// after the gap. remapXLSXRowNumbers instead replaces each row's
	// position-based number with its true spreadsheet row, read from the
	// worksheet's own <row r="..."> attributes.
	return remapXLSXRowNumbers(Normalize(kind, columns, dataRows), dataRowNums), nil
}

// resolveSheetPath turns a <sheet> entry from xl/workbook.xml into the zip
// entry name of its worksheet part, through xl/_rels/workbook.xml.rels as
// the design requires, rather than guessing a conventional sheetN.xml name.
func resolveSheetPath(sheet wbSheetXML, relTargets map[string]string) (string, error) {
	if sheet.RID == "" {
		return "", fmt.Errorf("sheet %q has no relationship id in xl/workbook.xml", sheet.Name)
	}
	target, ok := relTargets[sheet.RID]
	if !ok {
		return "", fmt.Errorf("sheet %q references relationship %q, not found in xl/_rels/workbook.xml.rels",
			sheet.Name, sheet.RID)
	}
	target = strings.TrimPrefix(target, "/")
	if !strings.HasPrefix(target, "xl/") {
		target = "xl/" + target
	}
	return path.Clean(target), nil
}

// joinRuns returns a shared-string or inline-string value: the plain text
// when present, otherwise its rich-text runs concatenated in order.
func joinRuns(plain string, runs []runXML) string {
	if plain != "" || len(runs) == 0 {
		return plain
	}
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.T)
	}
	return b.String()
}

// cellValue resolves one cell's textual value per its declared type.
// Numeric cells (type "n", or no type at all) and boolean cells (type "b")
// are returned as the literal text of <v>, untouched — formulas are not
// evaluated, so a "str" cell's cached result is passed through the same
// way. This is what lets an account id like "12345678" or Excel's
// "1.23457E+11" reach Normalize's repair/refuse logic exactly as written.
func cellValue(c cellXML, sst []string) (string, error) {
	switch c.T {
	case "s":
		if c.V == "" {
			return "", nil
		}
		idx, err := strconv.Atoi(c.V)
		if err != nil {
			return "", fmt.Errorf("cell %s: shared string index %q is not a number", c.R, c.V)
		}
		if idx < 0 || idx >= len(sst) {
			return "", fmt.Errorf("cell %s: shared string index %d out of range (have %d)", c.R, idx, len(sst))
		}
		return sst[idx], nil
	case "inlineStr":
		if c.Is == nil {
			return "", nil
		}
		return joinRuns(c.Is.T, c.Is.R), nil
	default: // "n" (default when T is empty), "b", "str", or anything else
		return c.V, nil
	}
}

// parseCellRef splits a cell reference such as "C1" or "AA57" into a
// 0-based column index and its 1-based row number.
func parseCellRef(ref string) (col, row int, err error) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		i++
	}
	if i == 0 || i == len(ref) {
		return 0, 0, fmt.Errorf("invalid cell reference %q", ref)
	}
	rowNum, err := strconv.Atoi(ref[i:])
	if err != nil || rowNum < 1 {
		return 0, 0, fmt.Errorf("invalid cell reference %q", ref)
	}
	col = 0
	for _, ch := range ref[:i] {
		col = col*26 + int(ch-'A'+1)
	}
	return col - 1, rowNum, nil
}

// rowsToGrid turns a worksheet's sparse <row>/<c> structure into a regular
// [][]string, placing every cell by the column its reference names (design:
// "sparse rows/cells must be placed by their cell reference ... not by
// position") rather than by its position within the XML. Every returned row
// is padded to the width of the widest row in the sheet so header and data
// rows line up by index the same way a text table's would.
//
// It also returns, parallel to grid, each row's true spreadsheet row number
// taken from the row's own r attribute -- never its position in rows. A real
// worksheet omits an entirely blank row from the XML altogether rather than
// emitting an empty <row> element (confirmed against files written by
// openpyxl, XlsxWriter and LibreOffice, package D2's real-file fixtures), so
// position and spreadsheet row number silently diverge after any such gap.
// A row missing its r attribute (not observed in any real file checked, but
// technically legal) falls back to one past the previous row's number, the
// same "fall back to position" rule cellValue's caller already applies to a
// <c> with no r attribute.
func rowsToGrid(sheetName string, rows []rowXML, sst []string) ([][]string, []int, error) {
	type parsedRow struct {
		cells  map[int]string
		maxCol int
		rowNum int
	}
	parsed := make([]parsedRow, 0, len(rows))
	overallMax := -1
	prevRowNum := 0

	for _, rw := range rows {
		rowNum := prevRowNum + 1
		if rw.R != "" {
			n, err := strconv.Atoi(rw.R)
			if err != nil || n < 1 {
				return nil, nil, fmt.Errorf("sheet %q: row has invalid r attribute %q", sheetName, rw.R)
			}
			rowNum = n
		}
		prevRowNum = rowNum

		cells := make(map[int]string, len(rw.Cells))
		maxCol := -1
		nextCol := 0
		for _, c := range rw.Cells {
			col := nextCol
			if c.R != "" {
				parsedCol, _, err := parseCellRef(c.R)
				if err != nil {
					return nil, nil, fmt.Errorf("sheet %q: %w", sheetName, err)
				}
				col = parsedCol
			}
			val, err := cellValue(c, sst)
			if err != nil {
				return nil, nil, fmt.Errorf("sheet %q: %w", sheetName, err)
			}
			cells[col] = val
			if col > maxCol {
				maxCol = col
			}
			nextCol = col + 1
		}
		if maxCol > overallMax {
			overallMax = maxCol
		}
		parsed = append(parsed, parsedRow{cells: cells, maxCol: maxCol, rowNum: rowNum})
	}

	width := overallMax + 1
	grid := make([][]string, len(parsed))
	rowNums := make([]int, len(parsed))
	for i, pr := range parsed {
		row := make([]string, width)
		for col, v := range pr.cells {
			row[col] = v
		}
		grid[i] = row
		rowNums[i] = pr.rowNum
	}
	return grid, rowNums, nil
}

// remapXLSXRowNumbers replaces Normalize's positional row numbers (1-based
// position within the dataRows slice it was given) with the true spreadsheet
// row each one actually came from, per dataRowNums. This is xlsx's
// equivalent of read.go's shiftRows, but a constant offset is not enough
// here: shiftRows is correct for CSV/TSV because a text table never omits a
// physical line (a blank line is still a line), while a real worksheet can
// omit an entirely blank row from its XML, shifting every position-based
// number after the gap out of alignment with the spreadsheet. Row 0 (a
// table-wide diagnostic, not tied to any data row) is left alone, exactly as
// shiftRows leaves it.
func remapXLSXRowNumbers(t Table, dataRowNums []int) Table {
	remap := func(pos int) int {
		if i := pos - 1; i >= 0 && i < len(dataRowNums) {
			return dataRowNums[i]
		}
		return pos // defensive: should be unreachable, never a worse answer than before
	}
	for i := range t.Diagnostics {
		if t.Diagnostics[i].Row > 0 {
			t.Diagnostics[i].Row = remap(t.Diagnostics[i].Row)
		}
	}
	for i := range t.Accounts {
		t.Accounts[i].SourceRow = remap(t.Accounts[i].SourceRow)
	}
	for i := range t.Networks {
		t.Networks[i].SourceRow = remap(t.Networks[i].SourceRow)
	}
	for i := range t.Ranges {
		t.Ranges[i].SourceRow = remap(t.Ranges[i].SourceRow)
	}
	return t
}

func firstNonBlankGridRow(grid [][]string) int {
	for i, row := range grid {
		for _, c := range row {
			if strings.TrimSpace(c) != "" {
				return i
			}
		}
	}
	return -1
}

// readZipPart reads one named part of an xlsx archive fully into memory,
// refusing anything over maxZipPartSize both by the archive's declared
// uncompressed size (a fast rejection) and by the actual bytes produced
// while decompressing (in case that declared size understates reality —
// the zip-bomb case the declared-size check alone would miss).
func readZipPart(zr *zip.Reader, name string) ([]byte, error) {
	var f *zip.File
	for _, cand := range zr.File {
		if cand.Name == name {
			f = cand
			break
		}
	}
	if f == nil {
		return nil, fmt.Errorf("%s not found in archive", name)
	}
	if f.UncompressedSize64 > maxZipPartSize {
		return nil, fmt.Errorf("%s: declares %d bytes uncompressed, over the %d byte per-part limit",
			name, f.UncompressedSize64, maxZipPartSize)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxZipPartSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > maxZipPartSize {
		return nil, fmt.Errorf("%s: exceeds the %d byte per-part limit while decompressing (possible zip bomb)",
			name, maxZipPartSize)
	}
	return data, nil
}
