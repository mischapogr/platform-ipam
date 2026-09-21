package onboard

import (
	"sort"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

// This file is ADR 0016's shared contributor construction. Package M9b1 built
// two copies of it: internal/onboardcmd's networkContributors (apply's create
// path) and, separately, mapNetworkRow for `onboard assess`. Package M9b2
// needs the same construction a third time, from Plan itself, to compare a
// table's rows against what a prefix already carries -- and a third
// independent copy is exactly how the two would drift, which is the failure
// ADR 0016 names by name ("the same string, not a similar one"). So the
// construction moves here, into the one package both Plan (below, in
// plan.go) and internal/onboardcmd's entryOccupancy can import without a
// cycle: onboardcmd already imports onboard, and onboard importing
// internal/assess is one-directional and safe -- internal/assess's own
// source-parsing test (TestImportsAreStandardLibraryOnly) keeps it free of
// every internal package including this one, and nothing in this package
// forbids the reverse. internal/onboardcmd keeps thin same-named wrappers
// (mapNetworkRow, networkContributors) so its own tests and its `onboard
// assess` command need no change.

// MapNetworkRow converts one NetworkRow to one assess.ResourceRecord, exactly
// as internal/onboardcmd's `onboard assess` command (package M1b3) already
// did before this package existed. AssociationID and ObservedAt are nil
// unless the row's own SOURCE FILE had that column at all
// (AssociationIDColumnPresent / ObservedAtColumnPresent) -- "no such column"
// and "column present, cell blank" both normalize to the empty string on
// NetworkRow, and only the column-presence flags can tell them apart; ADR
// 0016 requires that distinction to survive as an explicit null rather than
// be guessed away.
func MapNetworkRow(row NetworkRow) assess.ResourceRecord {
	rt := assess.ResourceType(strings.ToLower(strings.TrimSpace(row.Type)))

	var associationID *string
	if rt == assess.TypeVPC && row.AssociationIDColumnPresent {
		v := row.AssociationID
		associationID = &v
	}
	// A TypeSubnet record's AssociationID stays nil regardless of column
	// presence: assess.ResourceRecord's own doc comment states this is "nil
	// by construction ... not a degradation" -- a subnet is never an
	// association, so there is nothing here for the column to have recorded.

	var observedAt *string
	if row.ObservedAtColumnPresent {
		v := row.ObservedAt
		observedAt = &v
	}

	return assess.ResourceRecord{
		AccountID:     row.AccountID,
		Region:        row.Region,
		Type:          rt,
		ResourceID:    row.ResourceID,
		CIDR:          row.CIDR,
		AssociationID: associationID,
		Primary:       mapPrimary(row.Primary),
		ParentID:      row.ParentID,
		AZID:          row.AZID,
		Name:          row.Name,
		State:         row.State,
		ObservedAt:    observedAt,
		SourceFile:    row.SourceFile,
		SourceRow:     row.SourceRow,
	}
}

// mapPrimary normalizes a networks row's free-text "primary" cell to
// assess.Tri. ADR 0014 forbids ever guessing primary/secondary from a row's
// position or its prefix length; this only recognizes the literal words the
// collector itself writes, case-insensitively, so a cell this does not
// recognize -- including one that is simply blank -- becomes TriUnknown
// rather than a guess.
func mapPrimary(raw string) assess.Tri {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return assess.TriTrue
	case "false":
		return assess.TriFalse
	default:
		return assess.TriUnknown
	}
}

// ContributorsForRows builds ADR 0016's contributor list for one collapsed
// group of NetworkRows: one entry per source row, so that the identity of
// every VPC sharing a CIDR survives a collapse that used to keep only a
// sentence in the description. It is the ONE function both
// internal/onboardcmd's entryOccupancy (apply, on create) and Plan's
// contributor findings (this package, plan.go) call -- see this file's own
// doc comment for why that matters.
//
// batch is written into every entry's FirstSeenBatch/LastSeenBatch and
// otherwise never read by anything that compares entries: Plan has no batch
// of its own (only apply does, from --batch) and calls this with the empty
// string, which is safe because every finding Plan computes from the result
// compares Identity and ObservedAt only, never a batch name.
//
// Two normalizations, both of which ADR 0016's "never guessed" rule decides:
// an association id and an observation time are carried only when they are
// actually present AND non-empty. MapNetworkRow hands back a non-nil pointer
// to "" for a column that exists with a blank cell, and a blank cell is not
// an observation any more than a missing column is -- both are null here.
func ContributorsForRows(rows []NetworkRow, batch string) []domain.Contributor {
	if len(rows) == 0 {
		return nil
	}
	out := make([]domain.Contributor, 0, len(rows))
	for _, row := range rows {
		rec := MapNetworkRow(row)
		out = append(out, domain.Contributor{
			Identity:   rec.Identity(),
			AccountID:  row.AccountID,
			Region:     row.Region,
			Type:       string(rec.Type),
			ResourceID: row.ResourceID,
			ParentID:   row.ParentID,
			// first and last are the same batch on a create: this import is
			// both the one that first wrote the entry and the last one to
			// have refreshed it. Only a refresh (package M9b3) can ever move
			// them apart.
			AssociationID:  presentValue(rec.AssociationID),
			ObservedAt:     presentValue(rec.ObservedAt),
			FirstSeenBatch: batch,
			LastSeenBatch:  batch,
			SourceFile:     row.SourceFile,
			SourceRow:      row.SourceRow,
		})
	}
	// A total order, not the caller's: two files whose row numbers both
	// restart at 1 must not be interleaved by row number alone (M1b1 added
	// SourceFile for exactly that reason), and a re-import that only
	// reordered a table must not look to Plan's comparison like a changed
	// contributor set.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SourceFile != out[j].SourceFile {
			return out[i].SourceFile < out[j].SourceFile
		}
		if out[i].SourceRow != out[j].SourceRow {
			return out[i].SourceRow < out[j].SourceRow
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

// presentValue narrows MapNetworkRow's "the column existed" pointer to ADR
// 0016's "there is a value here" pointer: an empty string is dropped to null.
func presentValue(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	v := *p
	return &v
}
