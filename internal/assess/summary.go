package assess

import "fmt"

// forbiddenCompletePhrases is the deny list ADR 0014 names verbatim: "a test
// asserts that the output produced with complete false contains none of a
// forbidden list of substrings". It is checked by this package's own tests
// (report_test.go) against every rendering -- JSON and text -- of every
// incomplete-coverage fixture they generate, and is exported so package
// M1b3's own tests can reuse the exact same list rather than retyping it.
var forbiddenCompletePhrases = []string{
	"conflict-free",
	"no conflicts",
	"ready",
	"clean",
}

// ForbiddenCompletePhrases returns a copy of the deny list a Report with
// Coverage.Complete == false must never contain, in either encoding.
func ForbiddenCompletePhrases() []string {
	out := make([]string, len(forbiddenCompletePhrases))
	copy(out, forbiddenCompletePhrases)
	return out
}

func decisionCounts(conflicts []Conflict, decisions map[string]Decision) (decided, undecided, stale int) {
	ids := map[string]bool{}
	for _, c := range conflicts {
		ids[c.ID] = true
		if c.Decision != nil {
			decided++
		} else {
			undecided++
		}
	}
	for id := range decisions {
		if !ids[id] {
			stale++
		}
	}
	return decided, undecided, stale
}

// implicatedVPCCount is the number of distinct VPCs (account, region, vpc
// id) whose account, or whose specific account-region pair, appears among
// Coverage's failed, partial or not_attempted entries.
func implicatedVPCCount(coverage Coverage, survivors []ResourceRecord) int {
	wholeAccounts := map[string]bool{}
	pairs := map[AccountRegion]bool{}
	add := func(acct, region string) {
		if region == "" {
			wholeAccounts[acct] = true
			return
		}
		pairs[AccountRegion{AccountID: acct, Region: region}] = true
	}
	for _, f := range coverage.Failed {
		add(f.AccountID, f.Region)
	}
	for _, ar := range coverage.Partial {
		add(ar.AccountID, ar.Region)
	}
	for _, ar := range coverage.NotAttempted {
		add(ar.AccountID, ar.Region)
	}
	// RowCountShort (package M1c) is a genuine incomplete-read gap, exactly
	// like the three above; RowCountExceeded is deliberately not added --
	// see Coverage's own doc comment.
	for _, e := range coverage.RowCountShort {
		add(e.AccountID, e.Region)
	}

	vpcSet := map[string]bool{}
	for _, r := range survivors {
		if wholeAccounts[r.AccountID] || pairs[AccountRegion{AccountID: r.AccountID, Region: r.Region}] {
			vpcSet[r.vpcKey()] = true
		}
	}
	return len(vpcSet)
}

func computeSummary(coverage Coverage, conflicts []Conflict, duplicateObservations int, survivors []ResourceRecord, decisions map[string]Decision, stamp string) Summary {
	var total, fixedCount int
	byKind := map[Kind]int{}
	byImpact := map[Impact]int{}
	unknownOwnershipSides := 0
	for _, c := range conflicts {
		if c.Fixed {
			fixedCount++
		} else {
			total++
			byKind[c.Kind]++
			byImpact[c.Impact]++
		}
		for _, s := range c.Sides {
			if s.Owner == unknownOwnership {
				unknownOwnershipSides++
			}
		}
	}

	kindCounts := []KindCount{
		{Kind: KindEqualCIDR, Count: byKind[KindEqualCIDR]},
		{Kind: KindContains, Count: byKind[KindContains]},
	}
	impactCounts := []ImpactCount{
		{Impact: ImpactConfirmed, Count: byImpact[ImpactConfirmed]},
		{Impact: ImpactPotential, Count: byImpact[ImpactPotential]},
		{Impact: ImpactUnknown, Count: byImpact[ImpactUnknown]},
	}

	incompleteAccounts, incompleteRegionPairs := coverage.incompleteAccountsAndRegionPairs()
	implicated := implicatedVPCCount(coverage, survivors)
	decided, undecided, stale := decisionCounts(conflicts, decisions)

	sentence := renderSentence(coverage.Complete, total, fixedCount, byImpact, incompleteAccounts, incompleteRegionPairs, stamp)

	return Summary{
		Sentence:              sentence,
		TotalRelationships:    total,
		ByKind:                kindCounts,
		ByImpact:              impactCounts,
		FixedRelationships:    fixedCount,
		IncompleteAccounts:    incompleteAccounts,
		IncompleteRegionPairs: incompleteRegionPairs,
		ImplicatedVPCs:        implicated,
		DuplicateObservations: duplicateObservations,
		UnknownOwnershipSides: unknownOwnershipSides,
		DecisionsDecided:      decided,
		DecisionsUndecided:    undecided,
		DecisionsStale:        stale,
	}
}

// renderSentence implements ADR 0014's "exactly two summary templates"
// rule, selected by complete alone. The incomplete template always contains
// the exact substring "this report cannot make a complete statement for"
// and never contains the complete template's "no conflicting relationship
// was observed" sentence; see report_test.go for the test that checks the
// selection itself, not just one example.
func renderSentence(complete bool, total, fixedCount int, byImpact map[Impact]int, incompleteAccounts, incompleteRegionPairs int, stamp string) string {
	if !complete {
		gap := fmt.Sprintf("this report cannot make a complete statement for %d account(s) and %d account-region pair(s) because coverage is missing",
			incompleteAccounts, incompleteRegionPairs)
		if total == 0 && fixedCount == 0 {
			return gap + "; no VPC/CIDR relationship was found in the scope that was read, but an incomplete scope is not evidence that none exist"
		}
		return fmt.Sprintf("across the AWS network data actually observed, %d VPC/CIDR relationship(s) prevent the intended connectivity (%d confirmed, %d potential, %d unknown); %s",
			total, byImpact[ImpactConfirmed], byImpact[ImpactPotential], byImpact[ImpactUnknown], gap)
	}
	if total == 0 {
		sentence := "no conflicting relationship was observed in the scope read"
		if stamp != "" {
			sentence += " at " + stamp
		}
		return sentence
	}
	return fmt.Sprintf("across the AWS network data actually observed, %d VPC/CIDR relationship(s) prevent the intended connectivity (%d confirmed, %d potential, %d unknown)",
		total, byImpact[ImpactConfirmed], byImpact[ImpactPotential], byImpact[ImpactUnknown])
}
