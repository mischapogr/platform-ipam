package onboard

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// bom is the UTF-8 byte-order mark, built from byte values rather than
// written as a literal in the source: a raw BOM anywhere but the very
// start of a Go file is a syntax error ("illegal byte order mark").
var bom = string([]byte{0xEF, 0xBB, 0xBF})

// invisibleReplacer strips the characters spreadsheets and wikis leave
// behind: NBSP, zero-width space/joiners, a BOM, and curly quotes
// straightened to their ASCII equivalents. This runs before any other rule,
// per design section 4's first row.
var invisibleReplacer = strings.NewReplacer(
	" ", " ",
	"​", "",
	"‌", "",
	"‍", "",
	bom, "",
	"‘", "'",
	"’", "'",
	"“", "\"",
	"”", "\"",
)

func cleanCell(s string) string {
	return strings.TrimSpace(invisibleReplacer.Replace(s))
}

// cidrTokenPattern finds an IPv4 CIDR token inside a decorated cell such as
// "10.1.0.0/16 (prod)" or "VPC: 10.1.0.0/16".
var cidrTokenPattern = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}/\d{1,2}`)

// rangeDashPattern splits "10.0.0.1 - 10.0.0.50" into its two addresses.
// Matching alone does not decide a range: both sides must also parse as IP
// addresses, or the caller falls back to CIDR handling.
var rangeDashPattern = regexp.MustCompile(`^(.+?)\s*-\s*(.+)$`)

var scientificNotationPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[eE]\+?[0-9]+$`)
var allDigitsPattern = regexp.MustCompile(`^[0-9]+$`)

// headerHasColumn reports whether the resolved header included the given
// canonical column name at all -- the table-wide (one per Normalize call,
// i.e. one per input file) fact NetworkRow.AssociationIDColumnPresent and
// ObservedAtColumnPresent are built from. columns is ResolveHeaders' output:
// header index -> canonical column name (or the raw header text when no
// alias matched), so a column that resolved is a value of this map, never a
// key -- unlike splitKnown's values map, which is empty for a blank cell and
// therefore cannot answer this question.
func headerHasColumn(columns map[int]string, name string) bool {
	for _, c := range columns {
		if c == name {
			return true
		}
	}
	return false
}

// Normalize implements design section 4 for every row of the given kind,
// producing the surviving rows plus a Diagnostic for every rule that fired.
func Normalize(kind TableKind, columns map[int]string, rows [][]string) Table {
	table := Table{Kind: kind}
	known, ok := knownColumns[kind]
	if !ok {
		table.Diagnostics = append(table.Diagnostics, Diagnostic{
			Level: LevelError, Message: fmt.Sprintf("unknown table kind %q", kind),
		})
		return table
	}

	// Column presence is a property of this call's own header row, computed
	// once rather than per row: every row Normalize produces from these rows
	// came from the same input and therefore the same header (see
	// NetworkRow.AssociationIDColumnPresent's doc comment).
	assocPresent := headerHasColumn(columns, colAssociationID)
	observedPresent := headerHasColumn(columns, colObservedAt)

	for i, raw := range rows {
		sourceRow := i + 1
		cells := make([]string, len(raw))
		for j, c := range raw {
			cells[j] = cleanCell(c)
		}
		if allEmpty(cells) {
			continue // empty row: skipped
		}
		if isRepeatedHeaderRow(cells, columns) {
			continue // header row repeated by a page break: skipped
		}

		switch kind {
		case KindAccounts:
			row, diags := buildAccountRow(sourceRow, cells, columns, known)
			table.Diagnostics = append(table.Diagnostics, diags...)
			if row != nil {
				table.Accounts = append(table.Accounts, *row)
			}
		case KindNetworks:
			nrows, diags := buildNetworkRows(sourceRow, cells, columns, known, assocPresent, observedPresent)
			table.Diagnostics = append(table.Diagnostics, diags...)
			table.Networks = append(table.Networks, nrows...)
		case KindRanges:
			rrows, diags := buildRangeRows(sourceRow, cells, columns, known)
			table.Diagnostics = append(table.Diagnostics, diags...)
			table.Ranges = append(table.Ranges, rrows...)
		}
	}
	return table
}

func allEmpty(cells []string) bool {
	for _, c := range cells {
		if c != "" {
			return false
		}
	}
	return true
}

// isRepeatedHeaderRow detects a data row that is actually the header row
// again, as page breaks produce when a table is copied out of a document:
// every populated cell, run back through the same alias resolution used for
// the header row, names the same column it already has in columns.
func isRepeatedHeaderRow(cells []string, columns map[int]string) bool {
	matched := false
	for i, name := range columns {
		if i >= len(cells) {
			return false
		}
		cell := cells[i]
		if cell == "" {
			continue
		}
		matched = true
		key := normalizeHeaderKey(cell)
		if canon, ok := headerAliases[key]; ok {
			if canon != name {
				return false
			}
			continue
		}
		if key != normalizeHeaderKey(name) {
			return false
		}
	}
	return matched
}

// splitKnown walks a row left to right and separates cell values into the
// ones a known column, and free-text "Column: value" entries for everything
// else — a resolved-but-inapplicable canonical column (e.g. role_arn on a
// networks row) as much as a genuinely unrecognized header.
// columnLetter returns the spreadsheet letter of a zero-based column index
// (0 -> A, 25 -> Z, 26 -> AA).
func columnLetter(index int) string {
	letters := ""
	for n := index + 1; n > 0; n = (n - 1) / 26 {
		letters = string(rune('A'+(n-1)%26)) + letters
	}
	return letters
}

func splitKnown(cells []string, columns map[int]string, known map[string]bool) (map[string]string, []string) {
	values := make(map[string]string, len(known))
	var extras []string
	for i := 0; i < len(cells); i++ {
		v := cells[i]
		if v == "" {
			continue
		}
		name, ok := columns[i]
		if !ok {
			// A value with no header above it: the blank cell a merged header
			// leaves behind, or a row longer than the header row. It is still
			// the operator's data, so it is kept under the column's
			// spreadsheet letter rather than dropped without a trace.
			name = "column " + columnLetter(i)
		}
		if known[name] {
			if existing, ok := values[name]; ok {
				values[name] = existing + "; " + v
			} else {
				values[name] = v
			}
			continue
		}
		extras = append(extras, fmt.Sprintf("%s: %s", name, v))
	}
	return values, extras
}

func joinDescription(explicit, decoration string, extras []string) string {
	parts := make([]string, 0, 2+len(extras))
	if decoration != "" {
		parts = append(parts, decoration)
	}
	if explicit != "" {
		parts = append(parts, explicit)
	}
	parts = append(parts, extras...)
	return strings.Join(parts, "; ")
}

func splitRegions(s string) []string {
	if s == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || unicode.IsSpace(r)
	})
	return fields
}

// splitCIDRList splits a multi-value cell on comma, semicolon or newline
// (design section 4), but only at top level: a comma inside "(prod, primary)"
// decoration does not start a new CIDR.
func splitCIDRList(s string) []string {
	var parts []string
	depth := 0
	var cur strings.Builder
	for _, r := range s {
		switch r {
		case '(', '[':
			depth++
			cur.WriteRune(r)
		case ')', ']':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case ',', ';', '\n':
			if depth == 0 {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cleanDecoration(s string) string {
	s = strings.Trim(s, " \t\n()[]{}:;,-")
	return strings.Join(strings.Fields(s), " ")
}

func looksLikeIPv6(s string) bool {
	if !strings.Contains(s, ":") {
		return false
	}
	candidate := s
	if idx := strings.IndexByte(candidate, '/'); idx >= 0 {
		candidate = candidate[:idx]
	}
	candidate = strings.TrimSpace(candidate)
	addr, err := netip.ParseAddr(candidate)
	return err == nil && addr.Is6()
}

// processCIDRCell extracts and validates one CIDR token from a cell,
// applying: decoration extraction, the non-canonical-CIDR error, and the
// IPv6 warning. On success it returns the canonical CIDR and any decoration
// text found around it; on failure cidr is empty and diag is non-nil.
func processCIDRCell(row int, column, raw string) (cidr, decoration string, diag *Diagnostic) {
	s := cleanCell(raw)
	if s == "" {
		return "", "", nil
	}
	if m := cidrTokenPattern.FindString(s); m != "" {
		prefix, err := netip.ParsePrefix(m)
		if err != nil {
			return "", "", &Diagnostic{Level: LevelError, Row: row, Column: column,
				Message: fmt.Sprintf("%q is not a valid CIDR", m)}
		}
		if !prefix.Addr().Is4() {
			return "", "", &Diagnostic{Level: LevelWarning, Row: row, Column: column,
				Message: fmt.Sprintf("%q is an IPv6 value; v1 allocates IPv4 only, row skipped", m)}
		}
		if prefix != prefix.Masked() {
			return "", "", &Diagnostic{Level: LevelError, Row: row, Column: column,
				Message: fmt.Sprintf("%q is not a canonical CIDR (host bits set); refusing to mask it silently", m)}
		}
		remainder := cleanDecoration(strings.Replace(s, m, "", 1))
		return prefix.String(), remainder, nil
	}
	if looksLikeIPv6(s) {
		return "", "", &Diagnostic{Level: LevelWarning, Row: row, Column: column,
			Message: fmt.Sprintf("%q is an IPv6 value; v1 allocates IPv4 only, row skipped", s)}
	}
	return "", "", &Diagnostic{Level: LevelError, Row: row, Column: column,
		Message: fmt.Sprintf("%q does not contain a recognizable CIDR", s)}
}

// processRangeDash recognizes "10.0.0.1 - 10.0.0.50". It returns
// start == "" && diag == nil when the cell is not a dash-separated range at
// all, so the caller can fall back to CIDR handling.
func processRangeDash(row int, column, raw string) (start, end string, diag *Diagnostic) {
	s := cleanCell(raw)
	m := rangeDashPattern.FindStringSubmatch(s)
	if m == nil {
		return "", "", nil
	}
	a, errA := netip.ParseAddr(strings.TrimSpace(m[1]))
	b, errB := netip.ParseAddr(strings.TrimSpace(m[2]))
	if errA != nil || errB != nil {
		return "", "", nil
	}
	if a.Is6() || b.Is6() {
		return "", "", &Diagnostic{Level: LevelWarning, Row: row, Column: column,
			Message: fmt.Sprintf("%q is an IPv6 range; v1 allocates IPv4 only, row skipped", s)}
	}
	return a.String(), b.String(), nil
}

// normalizeAccountID applies the spreadsheet-damage repairs from design
// section 4: dashes and spaces are separators and are removed; a value that
// lost leading zeros (all digits, fewer than 12) is left-padded with a
// warning naming the row; Excel scientific notation is an error because the
// digits are already gone by the time the cell reaches us.
func normalizeAccountID(row int, raw string) (string, *Diagnostic) {
	s := cleanCell(raw)
	if s == "" {
		return "", nil
	}
	stripped := strings.NewReplacer("-", "", " ", "").Replace(s)
	if scientificNotationPattern.MatchString(stripped) {
		return "", &Diagnostic{Level: LevelError, Row: row, Column: colAccountID,
			Message: fmt.Sprintf("account id %q is in scientific notation; the original digits are lost", s)}
	}
	if allDigitsPattern.MatchString(stripped) && len(stripped) < 12 {
		padded := strings.Repeat("0", 12-len(stripped)) + stripped
		return padded, &Diagnostic{Level: LevelWarning, Row: row, Column: colAccountID,
			Message: fmt.Sprintf("account id %q has fewer than 12 digits; left-padded to %q", s, padded)}
	}
	return stripped, nil
}

func buildAccountRow(row int, cells []string, columns map[int]string, known map[string]bool) (*AccountRow, []Diagnostic) {
	values, extras := splitKnown(cells, columns, known)
	var diags []Diagnostic

	accountID := values[colAccountID]
	if accountID != "" {
		padded, diag := normalizeAccountID(row, accountID)
		if diag != nil {
			diags = append(diags, *diag)
			if diag.Level == LevelError {
				return nil, diags
			}
		}
		accountID = padded
	}

	r := &AccountRow{
		SourceRow:   row,
		AccountID:   accountID,
		AccountName: values[colAccountName],
		Environment: values[colEnvironment],
		TenantID:    values[colTenantID],
		Regions:     splitRegions(values[colRegions]),
		RoleARN:     values[colRoleARN],
		Owner:       values[colOwner],
		Description: joinDescription(values[colDescription], "", extras),
	}
	return r, diags
}

func buildNetworkRows(row int, cells []string, columns map[int]string, known map[string]bool, assocPresent, observedPresent bool) ([]NetworkRow, []Diagnostic) {
	values, extras := splitKnown(cells, columns, known)
	var diags []Diagnostic

	rawCIDR := values[colCIDR]
	if rawCIDR == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Row: row, Column: colCIDR,
			Message: "networks row has no cidr value"})
		return nil, diags
	}

	// A networks row carries an account id too, and it comes out of the same
	// spreadsheets with the same damage. The organization inventory CSV is a
	// networks table, so this is the table where the repair matters most.
	accountID := values[colAccountID]
	if accountID != "" {
		repaired, diag := normalizeAccountID(row, accountID)
		if diag != nil {
			diags = append(diags, *diag)
			if diag.Level == LevelError {
				return nil, diags
			}
		}
		accountID = repaired
	}

	associationID := values[colAssociationID]
	// ADR 0014: an observed_at that does not parse as RFC 3339 is preserved
	// verbatim in ObservedAt and is not an import error -- this package never
	// reads the parsed value, so there is nothing here for it to break. No
	// Diagnostic is raised either: a diagnostic would tell an operator to fix
	// data the import itself never looks at, which is exactly the kind of
	// noise design section 4 avoids elsewhere. The assessment (internal/assess)
	// is what turns an unparsable value into a reported limit.
	observedAt := values[colObservedAt]
	var observedAtParsed time.Time
	if observedAt != "" {
		if t, err := time.Parse(time.RFC3339, observedAt); err == nil {
			observedAtParsed = t
		}
	}

	var out []NetworkRow
	for _, part := range splitCIDRList(rawCIDR) {
		cidr, decoration, diag := processCIDRCell(row, colCIDR, part)
		if diag != nil {
			diags = append(diags, *diag)
			continue
		}
		if cidr == "" {
			continue
		}
		out = append(out, NetworkRow{
			SourceRow:                  row,
			CIDR:                       cidr,
			AccountID:                  accountID,
			Region:                     values[colRegion],
			Type:                       values[colType],
			ResourceID:                 values[colResourceID],
			ParentID:                   values[colParentID],
			AZID:                       values[colAZID],
			Name:                       values[colName],
			Environment:                values[colEnvironment],
			State:                      values[colState],
			Primary:                    values[colPrimary],
			Description:                joinDescription(values[colDescription], decoration, extras),
			AssociationID:              associationID,
			ObservedAt:                 observedAt,
			ObservedAtParsed:           observedAtParsed,
			AssociationIDColumnPresent: assocPresent,
			ObservedAtColumnPresent:    observedPresent,
		})
	}
	return out, diags
}

// rangeTokenToRow turns one already-split cell value from a ranges cidr
// column into a RangeRow, trying the dash-address-range form first.
func rangeTokenToRow(row int, token, desc, owner, source string, extras []string) (*RangeRow, *Diagnostic) {
	if start, end, diag := processRangeDash(row, colCIDR, token); start != "" || diag != nil {
		if diag != nil {
			return nil, diag
		}
		return &RangeRow{
			SourceRow: row, StartAddress: start, EndAddress: end,
			Description: joinDescription(desc, "", extras), Owner: owner, Source: source,
		}, nil
	}
	cidr, decoration, diag := processCIDRCell(row, colCIDR, token)
	if diag != nil {
		return nil, diag
	}
	if cidr == "" {
		return nil, nil
	}
	return &RangeRow{
		SourceRow: row, CIDR: cidr,
		Description: joinDescription(desc, decoration, extras), Owner: owner, Source: source,
	}, nil
}

func buildRangeRows(row int, cells []string, columns map[int]string, known map[string]bool) ([]RangeRow, []Diagnostic) {
	values, extras := splitKnown(cells, columns, known)
	var diags []Diagnostic
	desc, owner, source := values[colDescription], values[colOwner], values[colSource]

	if start, end := values[colStartAddress], values[colEndAddress]; start != "" || end != "" {
		s, sDiag := normalizeRangeAddress(row, colStartAddress, start)
		e, eDiag := normalizeRangeAddress(row, colEndAddress, end)
		if sDiag != nil {
			diags = append(diags, *sDiag)
		}
		if eDiag != nil {
			diags = append(diags, *eDiag)
		}
		if sDiag != nil || eDiag != nil {
			return nil, diags
		}
		return []RangeRow{{
			SourceRow: row, StartAddress: s, EndAddress: e,
			Description: joinDescription(desc, "", extras), Owner: owner, Source: source,
		}}, diags
	}

	rawCIDR := values[colCIDR]
	if rawCIDR == "" {
		diags = append(diags, Diagnostic{Level: LevelError, Row: row, Column: colCIDR,
			Message: "ranges row has neither cidr nor start/end address"})
		return nil, diags
	}

	var out []RangeRow
	for _, part := range splitCIDRList(rawCIDR) {
		r, diag := rangeTokenToRow(row, part, desc, owner, source, extras)
		if diag != nil {
			diags = append(diags, *diag)
			continue
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, diags
}

func normalizeRangeAddress(row int, column, raw string) (string, *Diagnostic) {
	s := cleanCell(raw)
	if s == "" {
		return "", &Diagnostic{Level: LevelError, Row: row, Column: column,
			Message: "range row is missing " + column}
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return "", &Diagnostic{Level: LevelError, Row: row, Column: column,
			Message: fmt.Sprintf("%q is not a valid IP address", s)}
	}
	if addr.Is6() {
		return "", &Diagnostic{Level: LevelWarning, Row: row, Column: column,
			Message: fmt.Sprintf("%q is IPv6; v1 allocates IPv4 only, row skipped", s)}
	}
	return addr.String(), nil
}
