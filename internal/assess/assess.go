package assess

import (
	"fmt"
	"sort"
)

// defaultMaxRelationships is --max-relationships' default (ADR 0014,
// "Scale"): "one hundred thousand, which refuses with exit 4 and names the
// count it would have produced rather than writing an unbounded file."
const defaultMaxRelationships = 100000

// TooManyRelationshipsError is returned by Assess when the sweep would
// produce more than Options.MaxRelationships (or defaultMaxRelationships)
// relationships. Package M1b3's command turns this into exit code 4.
type TooManyRelationshipsError struct {
	Count int
	Max   int
}

func (e *TooManyRelationshipsError) Error() string {
	return fmt.Sprintf("the sweep would produce %d relationships, more than --max-relationships %d; refusing rather than writing an unbounded report", e.Count, e.Max)
}

// Options is everything besides Input that shapes a Report: the reviewed
// inputs ADR 0014 calls gap M2's matrix and the ownership/fixed/decisions
// tables, the operator-supplied archival stamp, the relationship cap, and
// the exact bytes of every input file (for InputDigest; Assess performs no
// I/O to obtain them).
type Options struct {
	// Matrix is nil when no connectivity matrix was supplied: every
	// conflict is then Impact unknown (ADR 0014, "With no matrix the report
	// is still the useful artifact it must be").
	Matrix           *Matrix
	Ownership        Ownership
	Fixed            []FixedRange
	Decisions        map[string]Decision
	Stamp            string
	MaxRelationships int // 0 means defaultMaxRelationships
	InputFiles       []InputFile
}

// Assess builds a Report from in and opts. It is the package's only
// entry point that does real work end to end; every other exported function
// (DecodeMatrix, DecodeOwnership, DecodeFixed, DecodeDecisions, ConflictID)
// exists so a caller can build the two arguments this takes.
//
// Assess never calls time.Now, opens a file, calls NetBox or calls AWS. It
// returns an error in exactly two cases: *MissingFieldError (refuse: a
// required field is missing from some ResourceRecord) and
// *TooManyRelationshipsError (refuse: the sweep produced more relationships
// than the cap allows). Both map to package M1b3's exit code 4. Every other
// condition -- a missing association id, a missing observation time, a
// missing run.json, incomplete coverage, an unassignable connectivity group
// -- degrades the Report rather than erroring; see Report.InputLimits,
// Report.Coverage and Report.Notes.
func Assess(in Input, opts Options) (Report, error) {
	if err := Validate(in); err != nil {
		return Report{}, err
	}

	survivors, duplicateObservations := dedupeAssociations(in.Records)
	items, cidrNotes := buildAssociations(survivors, opts.Fixed)
	rels := sweep(items)

	max := opts.MaxRelationships
	if max <= 0 {
		max = defaultMaxRelationships
	}
	if len(rels) > max {
		return Report{}, &TooManyRelationshipsError{Count: len(rels), Max: max}
	}

	hasMatrix := opts.Matrix != nil
	var matrix Matrix
	if hasMatrix {
		matrix = *opts.Matrix
	}
	conflicts, groupNotes := buildConflicts(rels, opts.Ownership, matrix, hasMatrix, opts.Decisions)
	sortConflicts(conflicts)

	coverage := computeCoverage(in)
	inputLimits := computeInputLimits(survivors, in.Run, len(in.Accounts) > 0)
	summary := computeSummary(coverage, conflicts, duplicateObservations, survivors, opts.Decisions, opts.Stamp)

	notes := []Note{reachabilityNote()}
	notes = append(notes, cidrNotes...)
	notes = append(notes, subnetNotes(in.Records, survivors)...)
	notes = append(notes, vpcIDReuseNotes(survivors)...)
	notes = append(notes, groupNotes...)
	notes = append(notes, highFanoutNotes(items)...)
	notes = append(notes, rowCountExceededNotes(coverage.RowCountExceeded)...)
	sortNotes(notes)

	return Report{
		ReportVersion: reportVersion,
		Stamp:         opts.Stamp,
		Inputs:        buildDigests(opts.InputFiles),
		InputLimits:   inputLimits,
		Coverage:      coverage,
		Summary:       summary,
		Conflicts:     conflicts,
		Notes:         notes,
	}, nil
}

// sortNotes keeps NoteReachability first (the fixed, always-present
// disclaimer) and sorts everything else by kind, then message, then source
// file and row, for determinism.
func sortNotes(notes []Note) {
	sort.SliceStable(notes, func(i, j int) bool {
		a, b := notes[i], notes[j]
		aReach := a.Kind == NoteReachability
		bReach := b.Kind == NoteReachability
		if aReach != bReach {
			return aReach // the reachability disclaimer always sorts first
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		if a.SourceFile != b.SourceFile {
			return a.SourceFile < b.SourceFile
		}
		return a.SourceRow < b.SourceRow
	})
}

// Input limit names. Stable across releases.
const (
	LimitAssociationIDMissing   = "association-id-missing"
	LimitObservationTimeMissing = "observation-time-missing"
	LimitPrimaryMissing         = "primary-missing"
	LimitAttemptedSetUnknown    = "attempted-set-unknown"
	// LimitExpectedAccountsUnknown: no account list was supplied, so an account
	// the run never visited cannot be named and coverage is never complete.
	LimitExpectedAccountsUnknown = "expected-accounts-unknown"
	// LimitRowCountMissing (package M1c): run.json was supplied, but at
	// least one succeeded or partial attempt carries no row_count of its
	// own -- an older collector run (script_version 1, which predates
	// row_count) -- so that account/region's row_count cannot be
	// cross-checked against the rows present, never guessed at.
	LimitRowCountMissing = "row-count-missing"
)

// computeInputLimits names every degradation ADR 0014 requires to be named:
// a missing association_id, observed_at or primary column, and a missing
// run.json. It looks only at TypeVPC records (association id and primary
// are meaningless for a subnet row, which never carries either).
func computeInputLimits(survivors []ResourceRecord, run *RunRecord, accountsSupplied bool) []string {
	var assocMissing, observedMissing, primaryMissing bool
	for _, r := range survivors {
		if r.AssociationID == nil {
			assocMissing = true
		}
		if r.ObservedAt == nil {
			observedMissing = true
		}
		if r.primary() == TriUnknown {
			primaryMissing = true
		}
	}
	var limits []string
	if assocMissing {
		limits = append(limits, LimitAssociationIDMissing)
	}
	if observedMissing {
		limits = append(limits, LimitObservationTimeMissing)
	}
	if primaryMissing {
		limits = append(limits, LimitPrimaryMissing)
	}
	if run == nil {
		limits = append(limits, LimitAttemptedSetUnknown)
	} else {
		for _, a := range run.Attempts {
			if (a.Outcome == AttemptSucceeded || a.Outcome == AttemptPartial) && a.RowCount == nil {
				limits = append(limits, LimitRowCountMissing)
				break
			}
		}
	}
	if !accountsSupplied {
		limits = append(limits, LimitExpectedAccountsUnknown)
	}
	sort.Strings(limits)
	return limits
}
