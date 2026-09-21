package assess

import (
	"fmt"
	"math/rand"
	"net/netip"
	"testing"
	"time"
)

// TestCIDRBlocksNeverPartiallyOverlap is the property test ADR 0014 asks
// for: "say why partial overlap cannot occur between CIDR blocks and TEST
// it (property-style over generated prefixes)". A block of prefix length n
// is an interval of 2^(32-n) addresses aligned to a multiple of its own
// size; if two blocks share any address, the one with the longer prefix
// lies wholly inside the other. This generates many random prefix pairs and
// asserts exactly that: sharing any address implies full containment (or
// equality), never a partial overlap.
func TestCIDRBlocksNeverPartiallyOverlap(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const trials = 20000
	for i := 0; i < trials; i++ {
		aStart, aEnd := randomPrefixInterval(rng)
		bStart, bEnd := randomPrefixInterval(rng)

		sharesAddress := aStart <= bEnd && bStart <= aEnd
		if !sharesAddress {
			continue
		}
		aContainsB := aStart <= bStart && bEnd <= aEnd
		bContainsA := bStart <= aStart && aEnd <= bEnd
		if !aContainsB && !bContainsA {
			t.Fatalf("partial overlap found: a=[%d,%d] b=[%d,%d], neither contains the other", aStart, aEnd, bStart, bEnd)
		}
	}
}

func randomPrefixInterval(rng *rand.Rand) (start, end uint32) {
	bits := rng.Intn(25) // /0 .. /24, so blocks are large enough to collide often in the test
	size := uint64(1) << uint(32-bits)
	blockIndex := uint64(0)
	if size < 1<<32 {
		blockIndex = uint64(rng.Int63n(int64((uint64(1) << 32) / size)))
	}
	s := blockIndex * size
	e := s + size - 1
	return uint32(s), uint32(e)
}

func mustPrefix(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("parse %q: %v", cidr, err)
	}
	return p
}

func rec(account, region, vpcType, resourceID, cidr string, file string, row int) ResourceRecord {
	return ResourceRecord{
		AccountID: account, Region: region, Type: ResourceType(vpcType), ResourceID: resourceID, CIDR: cidr,
		SourceFile: file, SourceRow: row,
	}
}

func vpcRec(account, region, vpcID, cidr string, file string, row int) ResourceRecord {
	return rec(account, region, "vpc", vpcID, cidr, file, row)
}

// TestSweepMatchesNaiveComparison cross-checks the sort-and-sweep algorithm
// against a naive O(n^2) pairwise comparison on smaller random estates, as
// ADR 0014's evidence list and the M1b2 assignment both require.
func TestSweepMatchesNaiveComparison(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 200; trial++ {
		n := 5 + rng.Intn(30)
		var records []ResourceRecord
		for i := 0; i < n; i++ {
			account := fmt.Sprintf("%012d", rng.Intn(4))
			region := fmt.Sprintf("r%d", rng.Intn(3))
			vpc := fmt.Sprintf("vpc-%03d", i)
			bits := 16 + rng.Intn(9) // /16 .. /24
			base := rng.Intn(1 << 12)
			cidr := fmt.Sprintf("10.%d.0.0/%d", base%64, bits)
			// Re-derive a canonical CIDR by masking through netip so random
			// base/bits combinations are always valid.
			p, err := netip.ParsePrefix(cidr)
			if err != nil {
				continue
			}
			p = p.Masked()
			records = append(records, vpcRec(account, region, vpc, p.String(), "f", i+1))
		}

		survivors, _ := dedupeAssociations(records)
		items, _ := buildAssociations(survivors, nil)
		got := sweep(items)
		want := naivePairwise(items)

		if !relationshipSetsEqual(got, want) {
			t.Fatalf("trial %d: sweep and naive comparison disagree\n  sweep: %v\n  naive: %v", trial, relationshipKeys(got), relationshipKeys(want))
		}
	}
}

// naivePairwise is the O(n^2) reference implementation the sweep is checked
// against: every pair, in order, classified the same way the sweep does.
func naivePairwise(items []association) []rawRelationship {
	var out []rawRelationship
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if a.fixed && b.fixed {
				continue
			}
			if !a.fixed && !b.fixed && a.vpcKey == b.vpcKey {
				continue
			}
			sharesAddress := a.start <= b.end && b.start <= a.end
			if !sharesAddress {
				continue
			}
			if a.start == b.start && a.end == b.end {
				out = append(out, rawRelationship{kind: KindEqualCIDR, x: a, y: b})
				continue
			}
			if a.start <= b.start && b.end <= a.end {
				out = append(out, rawRelationship{kind: KindContains, x: a, y: b})
			} else {
				out = append(out, rawRelationship{kind: KindContains, x: b, y: a})
			}
		}
	}
	return out
}

func relationshipKeys(rels []rawRelationship) []string {
	var keys []string
	for _, r := range rels {
		keys = append(keys, string(r.kind)+"|"+ConflictID(r.kind, r.x.identity, r.y.identity))
	}
	return keys
}

func relationshipSetsEqual(a, b []rawRelationship) bool {
	ak, bk := relationshipKeys(a), relationshipKeys(b)
	if len(ak) != len(bk) {
		return false
	}
	counts := map[string]int{}
	for _, k := range ak {
		counts[k]++
	}
	for _, k := range bk {
		counts[k]--
	}
	for _, c := range counts {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestSweepScaleBound is ADR 0014's scale evidence: "ten thousand VPC
// associations and fifty thousand subnets across two hundred accounts must
// produce a report in under five seconds on one core." This package's own
// concern is the sweep and Assess as a whole, not the collector or the
// command, so the bound is measured over Assess directly against a
// generated synthetic estate; the bound used here (20s) is deliberately
// more generous than the record's 5s target so the test does not flake on a
// loaded machine, matching the M1b2 assignment's instruction.
func TestSweepScaleBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in -short mode")
	}
	in := genEstate(estateParams{
		accounts: 200, regionsPerAccount: 2, vpcsPerAccountRegion: 25,
		subnetsPerVPC: 12, conflictFraction: 0.05, seed: 7,
	})
	t.Logf("estate: %d resource records (%d VPC associations)", len(in.Records), countVPCRecords(in.Records))

	start := time.Now()
	report, err := Assess(in, Options{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	t.Logf("Assess over %d VPC associations produced %d conflicts in %v", countVPCRecords(in.Records), len(report.Conflicts), elapsed)
	if elapsed > 20*time.Second {
		t.Fatalf("Assess took %v, want well under 20s (ADR 0014 targets 5s for 10k associations/200 accounts)", elapsed)
	}
}

func countVPCRecords(records []ResourceRecord) int {
	n := 0
	for _, r := range records {
		if r.Type == TypeVPC {
			n++
		}
	}
	return n
}
