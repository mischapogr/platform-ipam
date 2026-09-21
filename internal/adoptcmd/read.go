package adoptcmd

// The reviewed adoption table's reader. internal/onboard's reader (package
// C2/C5) was considered and rejected: ReadTable/ReadText decide their output
// shape through the unexported ResolveHeaders, which is closed over exactly
// three fixed table kinds (accounts, networks, ranges) and none of them
// carries tenant_id, allocation_key or scope -- feeding this table through it
// would either misclassify it or fail to resolve any kind at all. Its
// Normalize also repairs and reinterprets cell values (CIDR decoration
// stripped, account ids re-padded, several CIDRs in one cell split into
// several rows) which is the opposite of what a reviewed adoption record
// needs: every field here must be exactly what the operator reviewed, or
// refused, never guessed at. So this is a second, smaller reader, not a
// second copy of onboard's: it keeps only the parts of onboard's approach
// that do fit -- encoding/csv with FieldsPerRecord=-1 for ragged rows, a
// leading UTF-8 BOM stripped before parsing, and delimiter selection by
// extension -- and none of the header aliasing or cell normalization, because
// this table's columns are fixed and its values are taken literally.

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// Canonical column names, in the order documented in service.go's package
// comment. Stable and exact: unlike onboard's alias table, a header must
// match one of these once trimmed and lowercased, or the whole file is
// refused before any row is read.
const (
	colTenantID       = "tenant_id"
	colAllocationKey  = "allocation_key"
	colScope          = "scope"
	colEnvironment    = "environment"
	colRegion         = "region"
	colAccountID      = "account_id"
	colCIDR           = "cidr"
	colResourceID     = "resource_id"
	colNetBoxPrefixID = "netbox_prefix_id"
	colParentKey      = "parent_allocation_key"
	colAZID           = "availability_zone_id"
)

// recordColumns lists every column a table must have, exactly once, and no
// others.
var recordColumns = []string{
	colTenantID, colAllocationKey, colScope, colEnvironment, colRegion, colAccountID,
	colCIDR, colResourceID, colNetBoxPrefixID, colParentKey, colAZID,
}

// Record is one reviewed row, taken from its table cells with no
// normalization beyond trimming surrounding whitespace. Field-level
// validation (a canonical CIDR, scope-conditional required fields) happens
// in requestFor, not here: this type is a faithful transcript of the row.
type Record struct {
	SourceRow           int
	TenantID            string
	AllocationKey       string
	Scope               string
	Environment         string
	Region              string
	AccountID           string
	CIDR                string
	ResourceID          string
	NetBoxPrefixID      string
	ParentAllocationKey string
	AvailabilityZoneID  string
}

// ReadRecords reads one reviewed table from r. name selects the delimiter by
// extension (".tsv" or ".txt" is tab-separated; anything else, including no
// extension, is comma-separated) and is used only to annotate errors and pick
// that delimiter -- it need not correspond to a real file.
func ReadRecords(name string, r io.Reader) ([]Record, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s: reading input: %w", name, err)
	}
	// A UTF-8 BOM before a quoted header reads as a bare quote inside an
	// unquoted field to encoding/csv, which then rejects the whole file --
	// the same trap internal/onboard/read.go documents and strips before
	// parsing, not after.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	ext := strings.ToLower(filepath.Ext(name))
	delim := ','
	if ext == ".tsv" || ext == ".txt" {
		delim = '\t'
	}

	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma = delim
	// Real tables are ragged: cell() below treats a short row as holding
	// empty values past its end rather than erroring on the first mismatch.
	cr.FieldsPerRecord = -1

	var rows [][]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rows = append(rows, rec)
	}

	idx := 0
	for idx < len(rows) && isBlankRow(rows[idx]) {
		idx++
	}
	if idx >= len(rows) {
		return nil, fmt.Errorf("%s: no header row found (file is empty or every row is blank)", name)
	}

	colIndex, err := resolveColumns(rows[idx])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	var out []Record
	for i, row := range rows[idx+1:] {
		if isBlankRow(row) {
			continue
		}
		// sourceRow counts the header itself and every physical line above
		// it, the same convention internal/onboard's readers use, so a row
		// number here is the line an operator sees in the file.
		sourceRow := idx + 2 + i
		rec := buildRecord(colIndex, row)
		rec.SourceRow = sourceRow
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no data rows", name)
	}
	return out, nil
}

// resolveColumns matches a header row against recordColumns exactly (after
// trimming and lowercasing): every column must be present exactly once, and
// no column outside the set is tolerated. This is the opposite of
// internal/onboard's ResolveHeaders, which keeps an unknown column as
// description text -- a supplied prefix_length or pool column here is a
// silent widening of a reviewed record, so it is refused instead of kept.
func resolveColumns(header []string) (map[string]int, error) {
	idxOf := make(map[string]int, len(recordColumns))
	known := make(map[string]bool, len(recordColumns))
	for _, c := range recordColumns {
		known[c] = true
	}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf(
				"unknown column %q; recognized columns are %s (prefix_length, address_family, pool and domain are derived and must not be supplied)",
				strings.TrimSpace(h), strings.Join(recordColumns, ", "))
		}
		if _, dup := idxOf[name]; dup {
			return nil, fmt.Errorf("column %q appears more than once", name)
		}
		idxOf[name] = i
	}
	var missing []string
	for _, c := range recordColumns {
		if _, ok := idxOf[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required column(s): %s", strings.Join(missing, ", "))
	}
	return idxOf, nil
}

func cell(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

func buildRecord(idxOf map[string]int, row []string) Record {
	get := func(c string) string { return cell(row, idxOf[c]) }
	return Record{
		TenantID:            get(colTenantID),
		AllocationKey:       get(colAllocationKey),
		Scope:               get(colScope),
		Environment:         get(colEnvironment),
		Region:              get(colRegion),
		AccountID:           get(colAccountID),
		CIDR:                get(colCIDR),
		ResourceID:          get(colResourceID),
		NetBoxPrefixID:      get(colNetBoxPrefixID),
		ParentAllocationKey: get(colParentKey),
		AvailabilityZoneID:  get(colAZID),
	}
}

func isBlankRow(row []string) bool {
	for _, f := range row {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}
