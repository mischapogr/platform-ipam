// Package onboard holds the canonical onboarding-import table types and the
// pure functions that turn a parsed spreadsheet into them (design section 4,
// docs/ONBOARDING_IMPORT.md). This package does no I/O: it never reads a
// file, calls NetBox, or loads configuration. Readers (package work item C2)
// and validation against configuration and a NetBox snapshot (C3) build on
// top of it.
package onboard

import "time"

// TableKind identifies which of the three canonical tables a parsed input
// resolves to, decided from which required columns are present
// (docs/ONBOARDING_IMPORT.md section 3).
type TableKind string

const (
	KindAccounts TableKind = "accounts"
	KindNetworks TableKind = "networks"
	KindRanges   TableKind = "ranges"
)

// Level is the severity of a Diagnostic.
type Level string

const (
	LevelInfo    Level = "info"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

// Diagnostic reports one condition raised while resolving headers or
// normalizing a row. Row is 1-based and counts source data rows only (the
// header row itself is never row 1); Row is 0 for a diagnostic that applies
// to the whole table, such as an unresolved required column. Column holds
// the canonical column name the diagnostic concerns.
type Diagnostic struct {
	Level   Level
	Row     int
	Column  string
	Message string
}

// AccountRow is one normalized row of an accounts table.
type AccountRow struct {
	SourceRow   int
	AccountID   string
	AccountName string
	Environment string
	TenantID    string
	Regions     []string
	RoleARN     string
	Owner       string
	// Description carries any explicit description/notes column, CIDR
	// decoration text, and every unknown column value: nothing read from
	// the input is ever dropped silently.
	Description string
}

// NetworkRow is one normalized row of a networks table. A source row whose
// CIDR cell held several networks (design section 4) produces one NetworkRow
// per CIDR, all sharing SourceRow.
type NetworkRow struct {
	SourceRow   int
	CIDR        string
	AccountID   string
	Region      string
	Type        string // "vpc" or "subnet", as given; not validated here
	ResourceID  string
	ParentID    string
	AZID        string
	Name        string
	Environment string
	State       string
	Primary     string
	Description string

	// AssociationID is the collector's association_id column (ADR 0014): the
	// VPC CIDR association's id, empty for a subnet row or when the column is
	// absent. Not validated here -- the assessment that reads it decides what
	// an absent or duplicated value means.
	AssociationID string
	// ObservedAt is the collector's observed_at cell, preserved exactly as
	// read: a value that fails to parse is NOT an import error (the import
	// never used it) and is kept verbatim so the assessment can report it as
	// a limit rather than silently discarding it. ObservedAtParsed is the
	// parsed instant when ObservedAt parses as RFC 3339, and the zero Time
	// otherwise -- callers must not treat a zero ObservedAtParsed as "no
	// value" without also checking ObservedAt.
	ObservedAt       string
	ObservedAtParsed time.Time
	// SourceFile names the input this row was read from. It is never set by
	// this package's own readers (read.go, xlsx.go): those are pure format
	// decoders with no opinion on what a caller will call their input, and
	// every one of their own existing tests constructs or reads NetworkRow
	// values with SourceFile at its zero value. internal/onboardcmd's parse
	// command sets it once, right after reading each input, which is also
	// where docs/WORK_PLAN.md's M1b1 block places the responsibility: rows
	// from several inputs concatenated by parse each restart SourceRow at 1,
	// so SourceFile is what tells them apart again.
	SourceFile string

	// AssociationIDColumnPresent and ObservedAtColumnPresent record whether
	// THIS ROW'S OWN INPUT had an association_id (resp. observed_at) column in
	// its header at all -- distinct from that column being present but this
	// row's own cell empty. AssociationID and ObservedAt above cannot make
	// that distinction on their own: both "no such column" and "column
	// present, cell blank" normalize to the empty string, and every subnet
	// row legitimately has an empty AssociationID even when the column
	// exists. docs/WORK_PLAN.md's M1b1 review flagged exactly this gap for
	// M1b3: "the table does not record which columns a file had, and the
	// engine needs 'column absent' (null, plus an input_limits entry) to
	// differ from 'present and empty'" -- internal/assess.ResourceRecord's
	// AssociationID and ObservedAt are *string for precisely that reason, and
	// package M1b3's command needs these two flags to decide, per row,
	// whether to map to nil or to a pointer.
	//
	// Set once per input by Normalize, from whether the resolved header
	// included the column -- a property of the row's own SOURCE FILE, not of
	// a merged Table: concatTables may combine an input written before ADR
	// 0014 (no such column at all) with one written after, and each row must
	// keep telling the truth about the file it actually came from. Meaningless
	// for AccountRow and RangeRow, which carry no such ambiguity today, so
	// this is added to NetworkRow only. Plan, Finding and WriteEntry never
	// read either field -- see the M1b1 report for why that scope was
	// deliberately left alone -- so this addition changes nothing about
	// Plan's output; TestPlanByteIdenticalForExistingFixtures (this package)
	// and the golden files it reads are the evidence.
	AssociationIDColumnPresent bool
	ObservedAtColumnPresent    bool
}

// RangeRow is one normalized row of a ranges table. Exactly one of CIDR or
// the StartAddress/EndAddress pair is set, depending on how the source row
// wrote the range.
type RangeRow struct {
	SourceRow    int
	CIDR         string
	StartAddress string
	EndAddress   string
	Description  string
	Owner        string
	Source       string
}

// Table is the normalized result of one parsed input. Only the field
// matching Kind is populated; the other two row slices stay nil.
type Table struct {
	Kind        TableKind
	Accounts    []AccountRow
	Networks    []NetworkRow
	Ranges      []RangeRow
	Diagnostics []Diagnostic
}
