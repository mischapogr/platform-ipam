package assess

import (
	"fmt"
	"sort"
)

// Note is one condition the assessment records that is neither a Conflict
// nor a Coverage entry: a data-quality anomaly, a scale warning, or the
// fixed, once-only reachability disclaimer every Report carries. Kind is a
// stable identifier, in the same spirit as internal/onboard's Rule
// constants.
type Note struct {
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	SourceFile string `json:"source_file,omitempty"`
	SourceRow  int    `json:"source_row,omitempty"`
}

// Note kinds. Stable across releases.
const (
	// NoteReachability is the fixed sentence ADR 0014 requires every Report
	// to print once: no route table, TGW attachment, propagation or traffic
	// test was read, and nothing in the report asserts that two workloads
	// can or cannot reach each other.
	NoteReachability = "reachability-disclaimer"

	// NoteInvalidCIDR: a present but non-canonical or non-IPv4 CIDR value,
	// excluded from every comparison (see buildAssociations).
	NoteInvalidCIDR = "invalid-cidr"

	// NoteSubnetParentMissing: a subnet's parent_id names no VPC resource id
	// anywhere in the input, so that VPC was not read at all (ADR 0014, "A
	// subnet is still read for two checks").
	NoteSubnetParentMissing = "subnet-parent-missing"

	// NoteSubnetOutsideParent: a subnet's CIDR lies outside every CIDR
	// association recorded for its own parent VPC, meaning that VPC's
	// association set was read incompletely.
	NoteSubnetOutsideParent = "subnet-outside-parent"

	// NoteVPCIDReusedAcrossAccounts: the same VPC id string appears under
	// more than one account id anywhere in the input. ADR 0014: "the same
	// VPC id read under two accounts is reported as a data error rather
	// than silently treated as one resource or two." It does not change how
	// the sweep compares the records -- account, region and VPC id together
	// remain the identity -- it only surfaces the anomaly.
	NoteVPCIDReusedAcrossAccounts = "vpc-id-reused-across-accounts"

	// NoteHighFanoutCIDR: one CIDR shared by many VPCs produces a
	// combinatorial number of relationships. ADR 0014, "Scale": "reported as
	// it is, with a note naming the CIDR and the count so a reader
	// understands the size."
	NoteHighFanoutCIDR = "high-fanout-cidr"

	// NoteAmbiguousGroup: a VPC matches more than one connectivity group's
	// members. ADR 0014: "not guessed at: the matrix is reported as
	// ambiguous for that VPC, and every conflict with that VPC on either
	// side is unknown."
	NoteAmbiguousGroup = "ambiguous-group-assignment"

	// NoteRowCountExceedsRecorded (package M1c): more rows are present for
	// an account and region than run.json's own row_count recorded for it.
	// A self-contradictory data error -- there is nowhere for the extra
	// rows to have come from if the collector's own count is correct -- so
	// it is reported as a note (docs/WORK_PLAN.md's M1c paragraph: "a data
	// error worth a note"), not folded into Coverage.RowCountShort's
	// incomplete-pairs count; Coverage.RowCountExceeded carries the same
	// pairs with their counts, structured, for a caller that wants them
	// without parsing this message.
	NoteRowCountExceedsRecorded = "row-count-exceeds-recorded"
)

// rowCountExceededNotes renders one Note per Coverage.RowCountExceeded
// entry, naming the account, region and the two counts that disagree.
func rowCountExceededNotes(entries []RowCountMismatchEntry) []Note {
	var notes []Note
	for _, e := range entries {
		notes = append(notes, Note{Kind: NoteRowCountExceedsRecorded,
			Message: fmt.Sprintf("account %s region %s: run.json recorded row_count %d, but %d rows are present in the networks input -- more than the collector itself counted for this account and region",
				e.AccountID, e.Region, e.Recorded, e.Present)})
	}
	return notes
}

// reachabilityNote is always the first entry of Report.Notes.
func reachabilityNote() Note {
	return Note{Kind: NoteReachability,
		Message: "no route table, Transit Gateway attachment, propagation or traffic test was read; nothing in this report asserts that any two workloads can or cannot reach each other"}
}

// subnetNotes implements ADR 0014's two subnet checks. It reads every
// TypeSubnet record against the deduplicated set of TypeVPC associations
// (subnets are never compared to each other and never appear in a
// Conflict).
func subnetNotes(records []ResourceRecord, vpcAssociations []ResourceRecord) []Note {
	// vpcExists: every (account, region, vpc id) triple that has at least
	// one surviving association.
	vpcExists := map[string]bool{}
	// byVPC: that triple's association CIDR intervals, for the containment
	// check.
	type ival struct{ start, end uint32 }
	byVPC := map[string][]ival{}
	for _, r := range vpcAssociations {
		key := r.vpcKey()
		vpcExists[key] = true
		if start, end, ok := cidrInterval(r.CIDR); ok {
			byVPC[key] = append(byVPC[key], ival{start, end})
		}
	}

	var notes []Note
	for _, r := range records {
		if r.Type != TypeSubnet {
			continue
		}
		parentKey := r.AccountID + "\x1f" + r.Region + "\x1f" + r.ParentID
		if !vpcExists[parentKey] {
			notes = append(notes, Note{Kind: NoteSubnetParentMissing,
				Message:    fmt.Sprintf("subnet %s in account %s region %s names parent VPC %s, which was not read", r.ResourceID, r.AccountID, r.Region, r.ParentID),
				SourceFile: r.SourceFile, SourceRow: r.SourceRow})
			continue
		}
		start, end, ok := cidrInterval(r.CIDR)
		if !ok {
			continue // already reported by buildAssociations for its own record set, but subnets are not fed there; note separately if invalid
		}
		inside := false
		for _, iv := range byVPC[parentKey] {
			if iv.start <= start && end <= iv.end {
				inside = true
				break
			}
		}
		if !inside {
			notes = append(notes, Note{Kind: NoteSubnetOutsideParent,
				Message:    fmt.Sprintf("subnet %s (%s) lies outside every CIDR association recorded for its parent VPC %s; that VPC's association set was read incompletely", r.ResourceID, r.CIDR, r.ParentID),
				SourceFile: r.SourceFile, SourceRow: r.SourceRow})
		}
	}
	return notes
}

// vpcIDReuseNotes flags a VPC id string observed under more than one
// account id.
func vpcIDReuseNotes(vpcAssociations []ResourceRecord) []Note {
	accountsByVPCID := map[string]map[string]bool{}
	var order []string
	for _, r := range vpcAssociations {
		if accountsByVPCID[r.ResourceID] == nil {
			accountsByVPCID[r.ResourceID] = map[string]bool{}
			order = append(order, r.ResourceID)
		}
		accountsByVPCID[r.ResourceID][r.AccountID] = true
	}
	sort.Strings(order)
	var notes []Note
	for _, vpcID := range order {
		accts := accountsByVPCID[vpcID]
		if len(accts) <= 1 {
			continue
		}
		var list []string
		for a := range accts {
			list = append(list, a)
		}
		sort.Strings(list)
		notes = append(notes, Note{Kind: NoteVPCIDReusedAcrossAccounts,
			Message: fmt.Sprintf("VPC id %s appears under %d different accounts (%v); treated as a data error, not as one resource or as independent ones", vpcID, len(accts), list)})
	}
	return notes
}

// highFanoutNotes flags a CIDR whose equal-cidr group is large enough that
// its pairwise relationship count is worth explaining, per ADR 0014
// "Scale". The threshold is not named by the record; this package uses 20
// members (190 pairs) as a defensible, narrow default -- see the report to
// the lead.
const highFanoutThreshold = 20

func highFanoutNotes(items []association) []Note {
	byCIDR := map[string][]association{}
	var order []string
	for _, a := range items {
		key := fmt.Sprintf("%d-%d", a.start, a.end)
		if _, ok := byCIDR[key]; !ok {
			order = append(order, key)
		}
		byCIDR[key] = append(byCIDR[key], a)
	}
	sort.Strings(order)
	var notes []Note
	for _, key := range order {
		group := byCIDR[key]
		if len(group) < highFanoutThreshold {
			continue
		}
		pairs := len(group) * (len(group) - 1) / 2
		notes = append(notes, Note{Kind: NoteHighFanoutCIDR,
			Message: fmt.Sprintf("CIDR %s is shared by %d resources, producing %d pairwise relationships", cidrString(group[0]), len(group), pairs)})
	}
	return notes
}

func cidrString(a association) string {
	if a.fixed {
		return a.fx.CIDR
	}
	return a.rec.CIDR
}
