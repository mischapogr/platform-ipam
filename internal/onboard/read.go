package onboard

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ReadOptions configures ReadTable and ReadText. Sheet selects a worksheet
// by name for .xlsx input (design section 3: "first sheet, or --sheet"); it
// is ignored for text formats.
type ReadOptions struct {
	Sheet string
}

// zipMagic is the four-byte signature every zip archive (including .xlsx,
// which is a zip container) starts with.
var zipMagic = [4]byte{0x50, 0x4B, 0x03, 0x04} // "PK\x03\x04"

// ReadTable turns one file-like input into a normalized Table, per
// docs/ONBOARDING_IMPORT.md section 3. name drives format detection by
// extension; ra must support random access (an *os.File does) because the
// xlsx central directory sits at the end of the archive. Text formats are
// read through an io.SectionReader over ra, so callers with only an
// io.Reader (stdin, package C5) should call ReadText directly instead.
func ReadTable(name string, ra io.ReaderAt, size int64, opts ReadOptions) (Table, error) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".xls", ".ods", ".pdf":
		return Table{}, fmt.Errorf(
			"%s: %s files are not supported; export the sheet to CSV (or .xlsx) and re-run", name, ext)
	case ".xlsx":
		if !hasZipMagic(ra) {
			return Table{}, fmt.Errorf(
				"%s: has an .xlsx extension but does not start with a zip signature; is this really an Excel file?", name)
		}
		return readXLSX(name, ra, size, opts)
	}
	return ReadText(name, io.NewSectionReader(ra, 0, size), opts)
}

// ReadText parses a text table — CSV, TSV, semicolon-delimited CSV, or a
// tab-separated Confluence paste — from r. name is used only to pick a
// delimiter (by extension, falling back to sniffing the header line) and to
// annotate errors; it need not correspond to a real file, so this is the
// entry point for stdin (package C5 passes name "-").
//
// A leading UTF-8 BOM or other invisible characters are not stripped here:
// ResolveHeaders and Normalize already clean every header and data cell
// through cleanCell (normalize.go), so handling it a second time here would
// just duplicate that rule — and risk the literal-BOM-in-source trap
// (docs/WORK_PLAN.md section 2) for no benefit.
func ReadText(name string, r io.Reader, opts ReadOptions) (Table, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Table{}, fmt.Errorf("%s: reading input: %w", name, err)
	}
	// Strip a UTF-8 byte-order mark before parsing, not after. Normalization
	// cleans it out of cells, but that is too late when the first header is
	// quoted: encoding/csv sees BOM + '"' as a bare quote inside an unquoted
	// field and rejects the whole file. That is exactly what a "CSV UTF-8"
	// export with quoted cells looks like.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	text := string(data)

	ext := strings.ToLower(filepath.Ext(name))
	delim := detectDelimiter(ext, firstNonBlankLine(text))

	cr := csv.NewReader(strings.NewReader(text))
	cr.Comma = delim
	// Real tables are ragged: a trailing optional column, an extra blank
	// cell from a merged header. FieldsPerRecord=-1 accepts any width per
	// row instead of erroring on the first mismatch; ResolveHeaders and
	// Normalize already tolerate rows shorter or longer than the header.
	cr.FieldsPerRecord = -1

	var records [][]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Table{}, fmt.Errorf("%s: %w", name, err)
		}
		records = append(records, rec)
	}

	// Skip leading blank lines (design section 4): encoding/csv only
	// treats a zero-length line as blank on its own, not one holding only
	// whitespace, so a row of empty/whitespace cells is skipped explicitly
	// here rather than being mistaken for the header.
	idx := 0
	for idx < len(records) && isBlankRecord(records[idx]) {
		idx++
	}
	if idx >= len(records) {
		return Table{}, fmt.Errorf("%s: no header row found (file is empty or every row is blank)", name)
	}

	headers := records[idx]
	dataRows := records[idx+1:]

	kind, columns, err := ResolveHeaders(headers)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", name, err)
	}
	return shiftRows(Normalize(kind, columns, dataRows), idx+1), nil
}

// shiftRows renumbers a normalized table so that every row number is the row
// an operator sees in the source: the header counts, and so do blank lines
// above it. Normalize numbers data rows from 1 because it never sees the
// header; left like that, every diagnostic points one row above the problem.
// Row 0 means "the table as a whole" and is left alone.
func shiftRows(t Table, offset int) Table {
	for i := range t.Diagnostics {
		if t.Diagnostics[i].Row > 0 {
			t.Diagnostics[i].Row += offset
		}
	}
	for i := range t.Accounts {
		t.Accounts[i].SourceRow += offset
	}
	for i := range t.Networks {
		t.Networks[i].SourceRow += offset
	}
	for i := range t.Ranges {
		t.Ranges[i].SourceRow += offset
	}
	return t
}

// firstNonBlankLine returns the first physical line of text that is not
// empty or all-whitespace, used only to sniff the delimiter. It splits on
// raw newlines rather than parsing CSV, so a header spanning a quoted
// embedded newline (not something real spreadsheets produce) would sniff
// against only its first line; that is an accepted limitation.
func firstNonBlankLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// detectDelimiter picks the field delimiter for a text input. Extension is
// authoritative for .tsv/.txt (tab) per design section 3. For .csv it
// defaults to comma, but a semicolon-majority header line overrides that:
// German Excel commonly exports "CSV" that is actually semicolon-delimited,
// and ONBOARDING_IMPORT.md calls that out by name as a case to support.
// Any other extension (including none, e.g. a Confluence paste or stdin)
// sniffs the header line by counting delimiter characters.
func detectDelimiter(ext, headerLine string) rune {
	tabs := strings.Count(headerLine, "\t")
	commas := strings.Count(headerLine, ",")
	semis := strings.Count(headerLine, ";")

	switch ext {
	case ".csv":
		if semis > commas {
			return ';'
		}
		return ','
	case ".tsv", ".txt":
		return '\t'
	default:
		switch {
		case tabs > commas && tabs > semis:
			return '\t'
		case semis > commas:
			return ';'
		default:
			return ','
		}
	}
}

func isBlankRecord(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

func hasZipMagic(ra io.ReaderAt) bool {
	var buf [4]byte
	n, err := ra.ReadAt(buf[:], 0)
	if n != len(buf) || (err != nil && err != io.EOF) {
		return false
	}
	return buf == zipMagic
}
