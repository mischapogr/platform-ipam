package assess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// reportVersion is Report.ReportVersion's value. Bump it whenever a JSON
// field is added, renamed or removed.
const reportVersion = 1

// KindCount and ImpactCount are Summary's by-kind and by-impact breakdowns.
// Both enumerations are small and fixed, so every Report always lists every
// value -- including a zero count -- rather than omitting one, which keeps
// the JSON shape identical across every report the tool can produce.
type KindCount struct {
	Kind  Kind `json:"kind"`
	Count int  `json:"count"`
}

type ImpactCount struct {
	Impact Impact `json:"impact"`
	Count  int    `json:"count"`
}

// Summary carries every count the rendered Sentence is built from, so a
// reader who disbelieves the sentence can recompute it from the fields
// beside it (ADR 0014, "The output").
type Summary struct {
	Sentence              string        `json:"sentence"`
	TotalRelationships    int           `json:"total_relationships"`
	ByKind                []KindCount   `json:"by_kind"`
	ByImpact              []ImpactCount `json:"by_impact"`
	FixedRelationships    int           `json:"fixed_relationships"`
	IncompleteAccounts    int           `json:"incomplete_accounts"`
	IncompleteRegionPairs int           `json:"incomplete_region_pairs"`
	ImplicatedVPCs        int           `json:"implicated_vpcs"`
	DuplicateObservations int           `json:"duplicate_observations"`
	UnknownOwnershipSides int           `json:"unknown_ownership_sides"`
	DecisionsDecided      int           `json:"decisions_decided"`
	DecisionsUndecided    int           `json:"decisions_undecided"`
	DecisionsStale        int           `json:"decisions_stale"`
}

// InputDigest names one input the report was built from and the SHA-256 of
// its exact bytes, "what makes two reports comparable and what lets a
// reader prove which bytes produced a conclusion" (ADR 0014).
type InputDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// InputFile is one input's path, as the caller named it, and its exact
// bytes. Assess never reads a file itself; a caller (package M1b3's
// command) reads every input and hands the bytes here so the report can
// name what produced it without this package doing any I/O of its own.
type InputFile struct {
	Path    string
	Content []byte
}

func buildDigests(files []InputFile) []InputDigest {
	digests := make([]InputDigest, 0, len(files))
	for _, f := range files {
		sum := sha256.Sum256(f.Content)
		digests = append(digests, InputDigest{Path: f.Path, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(digests, func(i, j int) bool { return digests[i].Path < digests[j].Path })
	return digests
}

// Report is the single value Assess produces. WriteReportJSON and
// WriteReportText both render it, so the machine form and the human form can
// never disagree (ADR 0014, "The output").
type Report struct {
	ReportVersion int           `json:"report_version"`
	Stamp         string        `json:"stamp,omitempty"`
	Inputs        []InputDigest `json:"inputs"`
	InputLimits   []string      `json:"input_limits"`
	Coverage      Coverage      `json:"coverage"`
	Summary       Summary       `json:"summary"`
	Conflicts     []Conflict    `json:"conflicts"`
	Notes         []Note        `json:"notes"`
}

// Clean reports whether the report is "clean" in the sense package M1b3's
// command needs to choose between exit 0 and exit 3: coverage is complete
// and no conflict is confirmed (ADR 0014, "Where it lives").
func (r Report) Clean() bool {
	if !r.Coverage.Complete {
		return false
	}
	for _, c := range r.Conflicts {
		if c.Impact == ImpactConfirmed {
			return false
		}
	}
	return true
}

// WriteReportJSON writes r as indented JSON with a trailing newline, exactly
// as internal/onboardcmd/drift.go's writeDriftReportJSON already does, so
// two runs over the same input produce byte-identical output.
func WriteReportJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteReportText writes r as a readable table for owners: the summary
// sentence, then one fixed-width row per conflict per side -- conflict id,
// kind, impact, then that side's account, VPC, CIDR, product and owner --
// then the coverage lists in full (ADR 0014, "The output").
func WriteReportText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintln(w, r.Summary.Sentence); err != nil {
		return err
	}
	fmt.Fprintf(w, "relationships: %d total (%d equal-cidr, %d contains), %d fixed; impact %d confirmed, %d potential, %d unknown\n",
		r.Summary.TotalRelationships, countKind(r.Summary.ByKind, KindEqualCIDR), countKind(r.Summary.ByKind, KindContains),
		r.Summary.FixedRelationships, countImpact(r.Summary.ByImpact, ImpactConfirmed), countImpact(r.Summary.ByImpact, ImpactPotential), countImpact(r.Summary.ByImpact, ImpactUnknown))
	fmt.Fprintf(w, "coverage: complete=%v, %d account(s) and %d account-region pair(s) without a complete statement, %d VPC(s) implicated\n",
		r.Coverage.Complete, r.Summary.IncompleteAccounts, r.Summary.IncompleteRegionPairs, r.Summary.ImplicatedVPCs)
	fmt.Fprintf(w, "duplicate observations collapsed: %d; sides with unknown ownership: %d\n", r.Summary.DuplicateObservations, r.Summary.UnknownOwnershipSides)
	fmt.Fprintf(w, "decisions: %d decided, %d undecided, %d stale (conflict id no longer appears)\n", r.Summary.DecisionsDecided, r.Summary.DecisionsUndecided, r.Summary.DecisionsStale)
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CONFLICT\tKIND\tIMPACT\tACCOUNT\tVPC\tCIDR\tPRODUCT\tOWNER")
	for _, c := range r.Conflicts {
		for _, s := range c.Sides {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Kind, c.Impact, s.AccountID, s.VPCID, s.CIDR, s.Product, s.Owner)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	if len(r.InputLimits) > 0 {
		fmt.Fprintln(w, "input limits:")
		for _, l := range r.InputLimits {
			fmt.Fprintf(w, "  - %s\n", l)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "coverage gaps:")
	fmt.Fprintf(w, "  failed: %d\n", len(r.Coverage.Failed))
	for _, f := range r.Coverage.Failed {
		fmt.Fprintf(w, "    %s %s %s: %s\n", f.AccountID, f.Region, f.Stage, f.Error)
	}
	fmt.Fprintf(w, "  partial: %d\n", len(r.Coverage.Partial))
	for _, p := range r.Coverage.Partial {
		fmt.Fprintf(w, "    %s %s\n", p.AccountID, p.Region)
	}
	fmt.Fprintf(w, "  not attempted: %d\n", len(r.Coverage.NotAttempted))
	for _, n := range r.Coverage.NotAttempted {
		fmt.Fprintf(w, "    %s %s\n", n.AccountID, n.Region)
	}
	fmt.Fprintf(w, "  read and empty: %d\n", len(r.Coverage.ReadEmpty))
	for _, e := range r.Coverage.ReadEmpty {
		fmt.Fprintf(w, "    %s %s\n", e.AccountID, e.Region)
	}
	fmt.Fprintf(w, "  row count short (fewer rows present than run.json recorded): %d\n", len(r.Coverage.RowCountShort))
	for _, m := range r.Coverage.RowCountShort {
		fmt.Fprintf(w, "    %s %s: recorded %d, present %d\n", m.AccountID, m.Region, m.Recorded, m.Present)
	}
	fmt.Fprintf(w, "  row count exceeded (more rows present than run.json recorded): %d\n", len(r.Coverage.RowCountExceeded))
	for _, m := range r.Coverage.RowCountExceeded {
		fmt.Fprintf(w, "    %s %s: recorded %d, present %d\n", m.AccountID, m.Region, m.Recorded, m.Present)
	}

	if len(r.Notes) > 0 {
		fmt.Fprintln(w, "notes:")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "  - [%s] %s\n", n.Kind, n.Message)
		}
	}
	return nil
}

func countKind(counts []KindCount, k Kind) int {
	for _, c := range counts {
		if c.Kind == k {
			return c.Count
		}
	}
	return 0
}

func countImpact(counts []ImpactCount, i Impact) int {
	for _, c := range counts {
		if c.Impact == i {
			return c.Count
		}
	}
	return 0
}
