package assess

import (
	"sort"
	"strconv"
)

// RowCountMismatchEntry is one account/region pair where run.json's own
// row_count for a succeeded or partial attempt disagrees with the rows
// actually present, in the networks input this run was given, for that
// account and region (package M1c, docs/WORK_PLAN.md: "the one known hole
// in ADR 0014's coverage guarantee -- run.json says how many rows each
// account and region produced ... and onboard assess never compares that
// with the rows actually present"). Present counts SOURCE rows -- every row
// of every --networks input attributed to this account and region, not the
// engine's own deduplicated associations (see rowCountMismatches).
type RowCountMismatchEntry struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	Recorded  int    `json:"recorded"`
	Present   int    `json:"present"`
}

func sortRowCountMismatches(list []RowCountMismatchEntry) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].AccountID != list[j].AccountID {
			return list[i].AccountID < list[j].AccountID
		}
		return list[i].Region < list[j].Region
	})
}

// AccountRegion is one account and (optionally empty, meaning the whole
// account) region entry inside Coverage.
type AccountRegion struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region,omitempty"`
}

// FailedEntry is one row of Coverage.Failed: one entry per failures.csv row.
type FailedEntry struct {
	AccountID   string       `json:"account_id"`
	AccountName string       `json:"account_name,omitempty"`
	Region      string       `json:"region,omitempty"`
	Stage       FailureStage `json:"stage"`
	Error       string       `json:"error"`
}

// Coverage is ADR 0014's first-class coverage result. Complete is the single
// mechanical field every summary template selects on: true if and only if
// Failed, Partial and NotAttempted are all empty and run.json was present.
type Coverage struct {
	Complete     bool            `json:"complete"`
	Failed       []FailedEntry   `json:"failed"`
	Partial      []AccountRegion `json:"partial"`
	NotAttempted []AccountRegion `json:"not_attempted"`
	ReadEmpty    []AccountRegion `json:"read_empty"`
	// RunMissing records that run.json itself was absent, which alone forces
	// Complete to false regardless of the other four lists (ADR 0014: "A
	// missing run.json means the coverage result carries
	// attempted-set-unknown and can never report complete").
	RunMissing bool `json:"run_missing"`
	// AccountsMissing records that no account list was supplied. The expected
	// set of accounts comes from accounts.json and from nowhere else, so
	// without it an account the run never visited cannot be named, and
	// Complete is false for the same reason a missing run.json makes it so.
	AccountsMissing bool `json:"accounts_missing"`
	// RowCountShort is package M1c: account/region pairs where FEWER rows
	// are present than run.json's own row_count recorded for that pair --
	// rows the collector believed it wrote are simply missing from what
	// this run was handed (a truncated or hand-filtered networks.csv beside
	// an intact run.json). This is a genuine incomplete read, of a
	// different kind than Failed/Partial/NotAttempted, so it is counted in
	// the summary's incomplete pairs exactly like them and forces Complete
	// to false.
	RowCountShort []RowCountMismatchEntry `json:"row_count_short"`
	// RowCountExceeded is package M1c's other direction: MORE rows are
	// present than run.json's own row_count recorded. There is nowhere for
	// the extra rows to have come from if the collector's own count is
	// correct, so this is a data error -- two networks inputs describing
	// one account/region under a single run record, most plausibly -- not
	// evidence that an account/region went unread. It is therefore NOT
	// counted in the summary's incomplete pairs (unlike RowCountShort), but
	// it still forces Complete to false: a report whose own inputs disagree
	// about how much was read cannot honestly claim completeness either.
	// Each entry also produces a Note (notes.go's rowCountExceededNotes)
	// naming it as the data error it is.
	RowCountExceeded []RowCountMismatchEntry `json:"row_count_exceeded"`
}

func sortAccountRegions(list []AccountRegion) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].AccountID != list[j].AccountID {
			return list[i].AccountID < list[j].AccountID
		}
		return list[i].Region < list[j].Region
	})
}

func dedupeAccountRegions(list []AccountRegion) []AccountRegion {
	sortAccountRegions(list)
	var out []AccountRegion
	for _, e := range list {
		if len(out) > 0 && out[len(out)-1] == e {
			continue
		}
		out = append(out, e)
	}
	return out
}

// computeCoverage builds Coverage from the account list, the failure rows
// and the run record, exactly as ADR 0014's "Coverage is a first-class
// result" section defines each list.
func computeCoverage(in Input) Coverage {
	var failed []FailedEntry
	// failedAccountRegion: every (account, region) named by a failure row,
	// used by "partial" below. A whole-account failure (empty region) is not
	// itself a partial-region entry.
	failedDescribeByAccountRegion := map[AccountRegion]bool{}
	accountRegionHasRows := map[AccountRegion]bool{}

	failureRecorded := map[AccountRegion]bool{}
	for _, f := range in.Failures {
		failureRecorded[AccountRegion{AccountID: f.AccountID, Region: f.Region}] = true
		failed = append(failed, FailedEntry{AccountID: f.AccountID, AccountName: f.AccountName, Region: f.Region, Stage: f.Stage, Error: f.Error})
		if f.Region != "" && f.Stage == StageDescribe {
			failedDescribeByAccountRegion[AccountRegion{AccountID: f.AccountID, Region: f.Region}] = true
		}
	}
	for _, r := range in.Records {
		if r.Type != TypeVPC {
			continue
		}
		accountRegionHasRows[AccountRegion{AccountID: r.AccountID, Region: r.Region}] = true
	}

	var partial []AccountRegion
	for ar := range failedDescribeByAccountRegion {
		if accountRegionHasRows[ar] {
			partial = append(partial, ar)
		}
	}
	activeAccounts := map[string]bool{}
	for _, a := range in.Accounts {
		if a.Status == "ACTIVE" {
			activeAccounts[a.AccountID] = true
		}
	}

	var notAttempted, readEmpty []AccountRegion
	runMissing := in.Run == nil

	if runMissing {
		// Only the account-level statement is available without run.json:
		// every ACTIVE account with no attempt recorded at all. Region-level
		// not_attempted needs the run record and is left empty, per ADR
		// 0014: "Where the region list is unknown the coverage lists say so
		// rather than inferring a region set."
		attemptedAccounts := map[string]bool{}
		for ar := range accountRegionHasRows {
			attemptedAccounts[ar.AccountID] = true
		}
		for _, f := range in.Failures {
			attemptedAccounts[f.AccountID] = true
		}
		for acct := range activeAccounts {
			if !attemptedAccounts[acct] {
				notAttempted = append(notAttempted, AccountRegion{AccountID: acct})
			}
		}
	} else {
		attemptedAccounts := map[string]bool{}
		for _, a := range in.Run.Attempts {
			attemptedAccounts[a.AccountID] = true
			switch a.Outcome {
			case AttemptNotAttempted:
				notAttempted = append(notAttempted, AccountRegion{AccountID: a.AccountID, Region: a.Region})
			case AttemptPartial:
				// The run record is evidence in its own right. failures.csv is
				// an optional input, and a report that called a partly read
				// region complete because that file was left out would be the
				// statement this whole result exists to prevent.
				partial = append(partial, AccountRegion{AccountID: a.AccountID, Region: a.Region})
			default:
				// A failed attempt, and equally an outcome this version does
				// not know: neither is a region that was read.
				if !failureRecorded[AccountRegion{AccountID: a.AccountID, Region: a.Region}] {
					stage := FailureStage(a.Stage)
					message := "recorded in " + runSource(in.Run) + " and in no failure row"
					if a.Outcome != AttemptFailed {
						message = "outcome " + strconv.Quote(string(a.Outcome)) + " in " + runSource(in.Run) + " is not one this version knows"
					}
					failed = append(failed, FailedEntry{AccountID: a.AccountID, Region: a.Region, Stage: stage, Error: message})
				}
			case AttemptSucceeded:
				if !accountRegionHasRows[AccountRegion{AccountID: a.AccountID, Region: a.Region}] {
					readEmpty = append(readEmpty, AccountRegion{AccountID: a.AccountID, Region: a.Region})
				}
			}
		}
		for acct := range activeAccounts {
			if !attemptedAccounts[acct] {
				notAttempted = append(notAttempted, AccountRegion{AccountID: acct})
			}
		}
	}
	partial = dedupeAccountRegions(partial)
	notAttempted = dedupeAccountRegions(notAttempted)
	readEmpty = dedupeAccountRegions(readEmpty)
	sort.Slice(failed, func(i, j int) bool {
		a, b := failed[i], failed[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		return a.Error < b.Error
	})

	accountsMissing := len(in.Accounts) == 0
	rowCountShort, rowCountExceeded := rowCountMismatches(in)
	complete := len(failed) == 0 && len(partial) == 0 && len(notAttempted) == 0 && !runMissing && !accountsMissing &&
		len(rowCountShort) == 0 && len(rowCountExceeded) == 0

	return Coverage{
		Complete:     complete,
		Failed:       failed,
		Partial:      partial,
		NotAttempted: notAttempted,
		ReadEmpty:    readEmpty,
		RunMissing:   runMissing,

		AccountsMissing:  accountsMissing,
		RowCountShort:    rowCountShort,
		RowCountExceeded: rowCountExceeded,
	}
}

// presentRowCounts counts, for every (account, region) pair, the SOURCE rows
// of the merged networks input attributed to it -- package M1c's rule is
// "count source rows, not deduplicated associations" (ADR 0014): a CIDR
// observed twice in one file is still two rows the collector wrote, and both
// count, which is why this reads in.Records (every row Validate accepted)
// rather than the engine's own deduplicated survivors. The one thing that
// must NOT count twice is the identical physical row read twice because the
// very same file was named more than once on the command line (an operator
// mistake, not a second collection): that is detected and collapsed by
// (source file, source row), the one identifier that names a specific
// physical row rather than what it says. Two DIFFERENT files that both
// happen to describe the same account and region are not collapsed --
// their rows are genuinely present and sum, which is exactly what should
// make a lone run.json's row_count look exceeded (RowCountExceeded) when
// that happens.
func presentRowCounts(records []ResourceRecord) map[AccountRegion]int {
	seen := map[string]bool{}
	counts := map[AccountRegion]int{}
	for _, r := range records {
		// A record that names no source cannot be recognised as the same
		// physical row as another, so it is never folded into one.
		if r.SourceFile != "" || r.SourceRow != 0 {
			key := r.SourceFile + "\x1f" + strconv.Itoa(r.SourceRow)
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		counts[AccountRegion{AccountID: r.AccountID, Region: r.Region}]++
	}
	return counts
}

// rowCountMismatches implements package M1c's cross-check: for every
// succeeded or partial RunAttempt that carries its own row_count, compare it
// with the rows actually present for that account and region.
//
// Two decisions this package makes, each tested:
//
//   - No run.json at all: rowCountMismatches returns nothing. That case is
//     already named by RunMissing/LimitAttemptedSetUnknown, unchanged from
//     before this package.
//   - An attempt whose RowCount is nil is skipped, never guessed at -- an
//     older run.json (script_version 1) predates row_count entirely, and
//     "never guess" is the same rule every other missing-column degradation
//     in this package follows. See LimitRowCountMissing in assess.go.
//   - An account/region present in the networks input but named by no
//     RunAttempt at all is skipped here too: there is no recorded row_count
//     to compare against, and whether that account/region was even meant to
//     be attempted is a different question the existing coverage lists
//     (built from accounts.json and run.json's own attempted set) already
//     answer.
func rowCountMismatches(in Input) (short, exceeded []RowCountMismatchEntry) {
	if in.Run == nil {
		return nil, nil
	}
	present := presentRowCounts(in.Records)
	for _, a := range in.Run.Attempts {
		if a.Outcome != AttemptSucceeded && a.Outcome != AttemptPartial {
			continue
		}
		if a.RowCount == nil {
			continue
		}
		recorded := *a.RowCount
		got := present[AccountRegion{AccountID: a.AccountID, Region: a.Region}]
		entry := RowCountMismatchEntry{AccountID: a.AccountID, Region: a.Region, Recorded: recorded, Present: got}
		switch {
		case got < recorded:
			short = append(short, entry)
		case got > recorded:
			exceeded = append(exceeded, entry)
		}
	}
	sortRowCountMismatches(short)
	sortRowCountMismatches(exceeded)
	return short, exceeded
}

func runSource(run *RunRecord) string {
	if run.SourceFile != "" {
		return run.SourceFile
	}
	return "the run record"
}

// incompleteAccountsAndRegionPairs returns the summary's two gap counts: the
// number of distinct accounts, and the number of distinct account-region
// pairs, for which no complete statement can be made -- the union of
// Failed, Partial and NotAttempted (ReadEmpty is a complete statement: "read
// and empty" is exactly what run.json lets the report say with confidence).
func (c Coverage) incompleteAccountsAndRegionPairs() (accounts int, regionPairs int) {
	accountSet := map[string]bool{}
	pairSet := map[AccountRegion]bool{}
	note := func(acctID, region string) {
		accountSet[acctID] = true
		if region != "" {
			pairSet[AccountRegion{AccountID: acctID, Region: region}] = true
		}
	}
	for _, f := range c.Failed {
		note(f.AccountID, f.Region)
	}
	for _, ar := range c.Partial {
		note(ar.AccountID, ar.Region)
	}
	for _, ar := range c.NotAttempted {
		note(ar.AccountID, ar.Region)
	}
	// RowCountShort (package M1c) is counted here exactly like the other
	// three: fewer rows present than recorded is a genuine incomplete read.
	// RowCountExceeded is deliberately NOT added: a surplus is a data error,
	// not evidence that an account/region went unread (see its own doc
	// comment on Coverage).
	for _, e := range c.RowCountShort {
		note(e.AccountID, e.Region)
	}
	return len(accountSet), len(pairSet)
}
