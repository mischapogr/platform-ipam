package assess

import (
	"encoding/binary"
	"net/netip"
	"sort"
)

// FixedRange is one row of the --fixed table: a non-AWS range (an
// on-premises network, a pool container, a partner range) that the ADR says
// enters the assessment only through this explicit input, never through
// loaded pools configuration.
type FixedRange struct {
	CIDR        string
	Description string
	Owner       string
	SourceFile  string
	SourceRow   int
}

// cidrInterval converts a canonical IPv4 CIDR into an inclusive
// [start, end] address interval. It mirrors internal/onboard/plan.go's own
// canonical-CIDR check (!p.Addr().Is4() || p != p.Masked()) so a value this
// package would refuse is refused the same way onboard already refuses one.
// IPv6 is out of scope: nothing in the organization inventory collects IPv6,
// and ADR 0014 is silent on it, so the narrowest reading is to treat it the
// same as any other malformed CIDR -- a data-error note, not a crash and not
// a refused report (see buildAssociations).
func cidrInterval(cidr string) (start, end uint32, ok bool) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return 0, 0, false
	}
	a4 := p.Addr().As4()
	start = binary.BigEndian.Uint32(a4[:])
	size := uint64(1) << uint(32-p.Bits())
	end = uint32(uint64(start) + size - 1)
	return start, end, true
}

// association is one item the sweep compares: either a TypeVPC
// ResourceRecord or a FixedRange, converted to an address interval. rec is
// the zero value when fixed is true, and fx is the zero value otherwise.
type association struct {
	start, end uint32
	vpcKey     string // "" and meaningless when fixed is true
	fixed      bool
	rec        ResourceRecord
	fx         FixedRange
	identity   string
}

func (a association) primary() Tri {
	if a.fixed {
		return TriUnknown
	}
	return a.rec.primary()
}

// dedupeKey groups records ADR 0014 says collapse to one observation:
// identical account, region, VPC id, CIDR and association id. Where the
// association id is absent the collapse uses the tuple without it, resting
// on the assumption the ADR states explicitly: AWS never associates one
// CIDR twice with the same VPC.
func dedupeKey(r ResourceRecord) string {
	assoc := ""
	if r.AssociationID != nil {
		assoc = *r.AssociationID
	}
	return r.AccountID + "\x1f" + r.Region + "\x1f" + r.ResourceID + "\x1f" + r.CIDR + "\x1f" + assoc
}

// less orders two source positions so the canonical survivor of a duplicate
// group is chosen the same way regardless of the order records arrived in --
// required for the determinism-across-input-order property.
func sourceLess(a, b ResourceRecord) bool {
	if a.SourceFile != b.SourceFile {
		return a.SourceFile < b.SourceFile
	}
	return a.SourceRow < b.SourceRow
}

// dedupeAssociations collapses duplicate observations among TypeVPC records
// and returns the canonical (lowest file, then lowest row) survivor of each
// group, plus the total number of rows collapsed away (ADR 0014, "Four
// things are not conflicts": "The same resource observed twice is not a
// conflict").
func dedupeAssociations(records []ResourceRecord) (survivors []ResourceRecord, duplicateObservations int) {
	groups := map[string][]ResourceRecord{}
	var order []string
	for _, r := range records {
		if r.Type != TypeVPC {
			continue
		}
		key := dedupeKey(r)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	sort.Strings(order)
	for _, key := range order {
		group := groups[key]
		best := group[0]
		for _, r := range group[1:] {
			if sourceLess(r, best) {
				best = r
			}
		}
		survivors = append(survivors, best)
		duplicateObservations += len(group) - 1
	}
	return survivors, duplicateObservations
}

// buildAssociations converts deduplicated VPC records and the fixed-range
// table into sweep items, reporting a data-error Note for any CIDR that does
// not parse as a canonical IPv4 prefix rather than refusing the whole
// report: ADR 0014's refusal list is exactly cidr/account_id/region/type/
// resource_id being *absent*; a *present but malformed* CIDR is not on that
// list, so the narrowest reading is the same "defence in depth, report and
// exclude" treatment internal/onboard/plan.go already gives a malformed
// CIDR under RuleInvalidCIDR. This is a decision the record did not make;
// see the package-level report to the lead.
func buildAssociations(records []ResourceRecord, fixed []FixedRange) (items []association, notes []Note) {
	for _, r := range records {
		start, end, ok := cidrInterval(r.CIDR)
		if !ok {
			notes = append(notes, Note{Kind: NoteInvalidCIDR,
				Message:    "resource " + r.ResourceID + " has CIDR " + r.CIDR + ", which is not a canonical IPv4 prefix; excluded from every comparison",
				SourceFile: r.SourceFile, SourceRow: r.SourceRow})
			continue
		}
		items = append(items, association{start: start, end: end, vpcKey: r.vpcKey(), rec: r, identity: r.Identity()})
	}
	// The order of the --fixed table's rows is not evidence, so neither a fixed
	// range's identity nor which of two rows naming one CIDR survives may turn
	// on it: the rows are put in an order of their own first, a CIDR named
	// twice is one range (the first in that order), and the identity is the
	// CIDR and nothing else. ADR 0014 does not name this case; an identity
	// taken from the row's position gave one conflict two ids.
	ordered := make([]FixedRange, len(fixed))
	copy(ordered, fixed)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		switch {
		case a.CIDR != b.CIDR:
			return a.CIDR < b.CIDR
		case a.Description != b.Description:
			return a.Description < b.Description
		case a.Owner != b.Owner:
			return a.Owner < b.Owner
		case a.SourceFile != b.SourceFile:
			return a.SourceFile < b.SourceFile
		}
		return a.SourceRow < b.SourceRow
	})
	seenFixed := map[string]bool{}
	for _, fx := range ordered {
		start, end, ok := cidrInterval(fx.CIDR)
		if !ok {
			notes = append(notes, Note{Kind: NoteInvalidCIDR,
				Message:    "fixed range " + fx.CIDR + " is not a canonical IPv4 prefix; excluded from every comparison",
				SourceFile: fx.SourceFile, SourceRow: fx.SourceRow})
			continue
		}
		if seenFixed[fx.CIDR] {
			continue
		}
		seenFixed[fx.CIDR] = true
		items = append(items, association{start: start, end: end, fixed: true, fx: fx, identity: fixedIdentity(fx)})
	}
	return items, notes
}

func fixedIdentity(fx FixedRange) string {
	return "fixed:" + fx.CIDR
}

// rawRelationship is one pair the sweep emits, before it is turned into a
// Conflict (which additionally needs ownership, the connectivity matrix and
// a stable id).
type rawRelationship struct {
	kind Kind
	// For KindContains, x is always the outer (containing) side and y is
	// always the inner (contained) side. For KindEqualCIDR neither role
	// applies; x and y are simply the pair, in sweep order.
	x, y association
}

// sweep is the sort-and-sweep algorithm ADR 0014's "Scale" section
// specifies: associations sorted by start ascending and end descending, so
// any block precedes every block it contains, then a single pass with a
// stack of still-open containers. Popping happens once per association's
// close, so the pass is O(n log n) for the sort plus O(n + output size) for
// the sweep itself -- the size of the output is not something an algorithm
// can reduce, because every reported relationship must be enumerated.
//
// The sort key includes each item's identity as a final tiebreak so the
// result is identical regardless of the order association records arrived
// in, which sortRelationships (report.go) then re-sorts into the report's
// own canonical order; the two sorts serve different purposes and are kept
// separate.
func sweep(items []association) []rawRelationship {
	sorted := make([]association, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.end != b.end {
			return a.end > b.end
		}
		return a.identity < b.identity
	})

	var open []association
	var rels []rawRelationship
	for _, cur := range sorted {
		kept := open[:0]
		for _, s := range open {
			if s.end >= cur.start {
				kept = append(kept, s)
			}
		}
		open = kept

		for _, s := range open {
			if s.fixed && cur.fixed {
				continue
			}
			if !s.fixed && !cur.fixed && s.vpcKey == cur.vpcKey {
				continue
			}
			if s.start == cur.start && s.end == cur.end {
				rels = append(rels, rawRelationship{kind: KindEqualCIDR, x: s, y: cur})
				continue
			}
			// s started at or before cur and s.end >= cur.start: since CIDR
			// blocks cannot partially overlap (TestCIDRBlocksNeverPartiallyOverlap),
			// sharing an address forces one to contain the other, and s (the
			// earlier-sorted, larger-or-equal block) is always the outer side.
			rels = append(rels, rawRelationship{kind: KindContains, x: s, y: cur})
		}
		open = append(open, cur)
	}
	return rels
}
