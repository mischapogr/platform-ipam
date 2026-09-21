package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// reportVersion is Report.ReportVersion's value. Bump it whenever a JSON
// field is added, renamed or removed (internal/assess/report.go's own
// convention).
const reportVersion = 1

// ApprovalResult, BlockerResult and VerificationResult carry a wave's or a
// move's typed input into the report verbatim (ADR 0015: "printed
// verbatim ... can never move a derived fact").
type ApprovalResult struct {
	ApprovedBy string `json:"approved_by,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
	Approves   string `json:"approves,omitempty"`
}

type BlockerResult struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type VerificationResult struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	RanBy       string `json:"ran_by,omitempty"`
	RanAt       string `json:"ran_at,omitempty"`
	Outcome     string `json:"outcome"`
}

// MoveResult is one planned move with every typed field it carried and
// every fact this package derived for it.
type MoveResult struct {
	// Subject is "aws:<account>:<region>:<vpc>" (Subject.identity).
	Subject   string `json:"subject"`
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	VPCID     string `json:"vpc_id"`

	// --- typed, carried verbatim ---
	Disposition   string               `json:"disposition"`
	WaveID        string               `json:"wave_id,omitempty"`
	Owner         string               `json:"owner,omitempty"`
	Approval      *ApprovalResult      `json:"approval,omitempty"`
	DependsOn     []string             `json:"depends_on,omitempty"`
	Blockers      []BlockerResult      `json:"blockers,omitempty"`
	Rollback      string               `json:"rollback,omitempty"`
	Verifications []VerificationResult `json:"verifications,omitempty"`
	Notes         string               `json:"notes,omitempty"`

	// --- derived ---
	SubjectFact       SubjectFact `json:"subject_fact"`
	SubjectFactReason string      `json:"subject_fact_reason,omitempty"`
	// Unmatched is ADR 0015's fourth answer for a subject that "appears in
	// no assessment at all": SubjectFact is Unknown whenever this is true
	// (see subjectFact in derive_facts.go), and this flag is what lets a
	// reader tell that specific cause apart from an ordinary coverage gap.
	Unmatched bool `json:"unmatched"`

	TargetFact TargetFact `json:"target_fact"`
	// TargetMismatchFields names the differing immutable fields when
	// TargetFact is TargetMismatched, sorted for determinism.
	TargetMismatchFields []string `json:"target_mismatch_fields,omitempty"`

	Resolves  []ResolveResult `json:"resolves,omitempty"`
	Unclaimed []string        `json:"unclaimed_conflicts,omitempty"`

	// FullyEvidenced is the one per-move aggregate ADR 0015 allows: target
	// active, subject not-observed, every claimed conflict stale, no
	// unclaimed conflict.
	FullyEvidenced bool `json:"fully_evidenced"`
	// KeepBlockedByConflict is set only for a `keep` disposition: true while
	// the current assessment still reports any conflict touching this
	// subject, which ADR 0010's reviewedOccupancy refuses to adopt (ADR
	// 0015, "Relation to reservations, to adoption ...").
	KeepBlockedByConflict bool `json:"keep_blocked_by_conflict,omitempty"`
}

// WaveRollup is one wave's roll-up: counts per fact, side by side, each
// with its own denominator (MovesTotal), never summed with the typed count
// block (ADR 0015, "What is derived").
type WaveRollup struct {
	ID             string          `json:"id"`
	Name           string          `json:"name,omitempty"`
	Owner          string          `json:"owner,omitempty"`
	Approval       *ApprovalResult `json:"approval,omitempty"`
	MovesTotal     int             `json:"moves_total"`
	FullyEvidenced int             `json:"fully_evidenced"`
	BySubjectFact  []FactCount     `json:"by_subject_fact"`
	ByTargetFact   []FactCount     `json:"by_target_fact"`
}

// FactCount is one enumeration value and its count. Every enumeration this
// package counts is small and fixed (subject fact, target fact,
// disposition), so a Report always lists every value, including a zero
// count, exactly as internal/assess.KindCount/ImpactCount do -- "the JSON
// shape [stays] identical across every report the tool can produce."
type FactCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// DerivedCounts is the report's DERIVED count block (ADR 0015: "Derived
// counts and typed counts are printed in two separate blocks and are never
// added together").
type DerivedCounts struct {
	MovesTotal         int         `json:"moves_total"`
	FullyEvidenced     int         `json:"fully_evidenced"`
	BySubjectFact      []FactCount `json:"by_subject_fact"`
	ByTargetFact       []FactCount `json:"by_target_fact"`
	ResolvedPresent    int         `json:"resolved_present"`
	ResolvedStale      int         `json:"resolved_stale"`
	UnclaimedConflicts int         `json:"unclaimed_conflicts"`
	UnplannedConflicts int         `json:"unplanned_conflicts"`
	UnmatchedSubjects  int         `json:"unmatched_subjects"`
}

// TypedCounts is the report's TYPED count block: what reviewers typed,
// counted without deriving anything from it (ADR 0015: "an approved wave
// and a completed one are different facts").
type TypedCounts struct {
	MovesByDisposition     []FactCount `json:"moves_by_disposition"`
	MovesApproved          int         `json:"moves_approved"`
	WavesTotal             int         `json:"waves_total"`
	WavesApproved          int         `json:"waves_approved"`
	VerificationsByOutcome []FactCount `json:"verifications_by_outcome,omitempty"`
	BlockersTotal          int         `json:"blockers_total"`
	KeepBlockedByConflict  int         `json:"keep_blocked_by_conflict"`
}

// Summary carries the rendered Sentence (selected by one of exactly two
// templates, report_templates.go) and both count blocks.
type Summary struct {
	Sentence string        `json:"sentence"`
	Derived  DerivedCounts `json:"derived"`
	Typed    TypedCounts   `json:"typed"`
}

// EmbeddedAssessment carries the assessment's own Inputs, InputLimits,
// Coverage and Summary verbatim (ADR 0015: "The progress report carries the
// assessment's inputs, input_limits, coverage and summary verbatim, so a
// reader sees both halves of one snapshot, and does not duplicate the
// conflict list, which onboard assess already prints").
type EmbeddedAssessment struct {
	Inputs      []assess.InputDigest `json:"inputs"`
	InputLimits []string             `json:"input_limits"`
	Coverage    assess.Coverage      `json:"coverage"`
	Summary     assess.Summary       `json:"summary"`
}

// EvidenceReport is the report's `evidence` block (ADR 0015: "its complete
// flag, the scopes, the read instants and the digests").
type EvidenceReport struct {
	Complete     bool                 `json:"complete"`
	Scopes       []string             `json:"scopes"`
	ReadInstants []string             `json:"read_instants"`
	Digests      []assess.InputDigest `json:"digests"`
}

// Report is the single value Derive produces. WriteReportJSON and
// WriteReportText both render it, so the machine form and the human form
// cannot disagree (ADR 0015, mirroring ADR 0014's Report).
type Report struct {
	ReportVersion int                  `json:"report_version"`
	Stamp         string               `json:"stamp,omitempty"`
	Inputs        []assess.InputDigest `json:"inputs"`
	InputLimits   []string             `json:"input_limits"`
	Assessment    EmbeddedAssessment   `json:"assessment"`
	Evidence      EvidenceReport       `json:"evidence"`
	Summary       Summary              `json:"summary"`
	Waves         []WaveRollup         `json:"waves"`
	Moves         []MoveResult         `json:"moves"`
	// Unplanned lists, sorted, the id of every conflict the current
	// assessment reports that touches no move's subject at all (ADR 0015,
	// "What is derived").
	Unplanned []string `json:"unplanned_conflicts"`
	// Notes carries the two fixed, once-only disclaimers ADR 0015 requires
	// (the assessment's own reachability disclaimer, verbatim, and this
	// report's own scope disclaimer -- report_templates.go's
	// disclaimerNotes), always in that order.
	Notes []string `json:"notes"`
}

// buildTopLevelDigests merges the plan files' digests with the evidence
// files' digests into Report.Inputs, sorted by path -- the same rule
// assess.buildDigests uses, so "every plan file is listed in the report's
// inputs with the SHA-256 of its bytes" (ADR 0015) holds for the evidence
// files too, since the record's own JSON layout names one top-level
// `inputs` list rather than a plan-only one.
func buildTopLevelDigests(planFiles []assess.InputFile, evidenceFiles []DerivedEvidenceFile) []assess.InputDigest {
	out := make([]assess.InputDigest, 0, len(planFiles)+len(evidenceFiles))
	for _, f := range planFiles {
		sum := sha256.Sum256(f.Content)
		out = append(out, assess.InputDigest{Path: f.Path, SHA256: hex.EncodeToString(sum[:])})
	}
	for _, f := range evidenceFiles {
		sum := sha256.Sum256(f.Content)
		out = append(out, assess.InputDigest{Path: f.Path, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// buildSummary computes both count blocks and selects the rendered
// sentence (report_templates.go). targetTenants is the plan's own set of
// named target tenants (derive.go's targetTenantSet), passed through so the
// incomplete template can name exactly which tenants no evidence could have
// seen, without recomputing it from idx (which only knows what scopes it
// was given, not what the plan asked for).
func buildSummary(moves []MoveResult, waves []WaveRollup, unplanned []string, evComplete bool, report assess.Report, idx *evidenceIndex, targetTenants []string, stamp string) Summary {
	derived := DerivedCounts{MovesTotal: len(moves)}
	bySubject := map[SubjectFact]int{}
	byTarget := map[TargetFact]int{}
	dispositionCounts := map[Disposition]int{}
	movesApproved := 0
	verificationOutcomes := map[string]int{}
	blockersTotal := 0
	keepBlocked := 0

	for _, m := range moves {
		bySubject[m.SubjectFact]++
		byTarget[m.TargetFact]++
		if m.FullyEvidenced {
			derived.FullyEvidenced++
		}
		if m.Unmatched {
			derived.UnmatchedSubjects++
		}
		derived.UnclaimedConflicts += len(m.Unclaimed)
		for _, r := range m.Resolves {
			if r.Status == ResolvePresent {
				derived.ResolvedPresent++
			} else {
				derived.ResolvedStale++
			}
		}

		dispositionCounts[Disposition(m.Disposition)]++
		if m.Approval != nil {
			movesApproved++
		}
		for _, v := range m.Verifications {
			verificationOutcomes[v.Outcome]++
		}
		blockersTotal += len(m.Blockers)
		if m.KeepBlockedByConflict {
			keepBlocked++
		}
	}
	derived.UnplannedConflicts = len(unplanned)
	derived.BySubjectFact = subjectFactCounts(bySubject)
	derived.ByTargetFact = targetFactCounts(byTarget)

	wavesApproved := 0
	for _, w := range waves {
		if w.Approval != nil {
			wavesApproved++
		}
	}

	typed := TypedCounts{
		MovesByDisposition: []FactCount{
			{Value: string(DispositionReplace), Count: dispositionCounts[DispositionReplace]},
			{Value: string(DispositionKeep), Count: dispositionCounts[DispositionKeep]},
			{Value: string(DispositionRetire), Count: dispositionCounts[DispositionRetire]},
			{Value: string(DispositionUndecided), Count: dispositionCounts[DispositionUndecided]},
		},
		MovesApproved:         movesApproved,
		WavesTotal:            len(waves),
		WavesApproved:         wavesApproved,
		BlockersTotal:         blockersTotal,
		KeepBlockedByConflict: keepBlocked,
	}
	if len(verificationOutcomes) > 0 {
		var outcomes []string
		for o := range verificationOutcomes {
			outcomes = append(outcomes, o)
		}
		sort.Strings(outcomes)
		for _, o := range outcomes {
			typed.VerificationsByOutcome = append(typed.VerificationsByOutcome, FactCount{Value: o, Count: verificationOutcomes[o]})
		}
	}

	uncoveredTenants := evidenceScopeIncompleteFor(idx, targetTenants)
	sentence := renderSentence(evComplete, len(moves), derived.FullyEvidenced,
		report.Summary.IncompleteAccounts, report.Summary.IncompleteRegionPairs, uncoveredTenants, len(idx.files) == 0, stamp)

	return Summary{Sentence: sentence, Derived: derived, Typed: typed}
}

// WriteReportJSON writes r as indented JSON with a trailing newline (ADR
// 0015, "Determinism, the output and confidentiality": "the encoder is
// encoding/json with two-space indentation and a trailing newline").
func WriteReportJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
