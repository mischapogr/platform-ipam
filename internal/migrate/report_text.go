package migrate

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// WriteReportText writes r as a readable table (ADR 0015, "Determinism, the
// output and confidentiality": "the text form renders the same value: the
// selected summary sentence, the derived counts, the typed counts, a
// fixed-width table of moves -- subject, disposition, wave, owner, then the
// three derived facts -- the wave roll-ups, the unplanned conflicts, and
// the notes in full"). Every label below is deliberately spelled to avoid
// every word in forbiddenPhrases (see that variable's own doc comment for
// why "complete" is not one of them here even though it is spelled out as a
// literal JSON field name).
func WriteReportText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintln(w, r.Summary.Sentence); err != nil {
		return err
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, n)
	}
	fmt.Fprintln(w)

	d := r.Summary.Derived
	fmt.Fprintf(w, "derived: %d move(s), %d fully evidenced\n", d.MovesTotal, d.FullyEvidenced)
	fmt.Fprintf(w, "  subject: %s\n", factCountsLine(d.BySubjectFact))
	fmt.Fprintf(w, "  target:  %s\n", factCountsLine(d.ByTargetFact))
	fmt.Fprintf(w, "  conflicts resolved: %d present, %d stale; unclaimed %d; unplanned %d; unmatched subjects %d\n",
		d.ResolvedPresent, d.ResolvedStale, d.UnclaimedConflicts, d.UnplannedConflicts, d.UnmatchedSubjects)
	fmt.Fprintln(w)

	t := r.Summary.Typed
	fmt.Fprintf(w, "typed: %s\n", factCountsLine(t.MovesByDisposition))
	fmt.Fprintf(w, "  approvals: %d of %d move(s), %d of %d wave(s)\n", t.MovesApproved, d.MovesTotal, t.WavesApproved, t.WavesTotal)
	fmt.Fprintf(w, "  blockers: %d; keep moves blocked by a still-reported conflict: %d\n", t.BlockersTotal, t.KeepBlockedByConflict)
	if len(t.VerificationsByOutcome) > 0 {
		fmt.Fprintf(w, "  verifications: %s\n", factCountsLine(t.VerificationsByOutcome))
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "evidence: scope covers every named tenant: %v; scopes %v; read at %v\n",
		r.Evidence.Complete, r.Evidence.Scopes, r.Evidence.ReadInstants)
	if len(r.InputLimits) > 0 {
		fmt.Fprintln(w, "input limits:")
		for _, l := range r.InputLimits {
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SUBJECT\tDISPOSITION\tWAVE\tOWNER\tSUBJECT_FACT\tTARGET_FACT\tCONFLICTS\tFULLY_EVIDENCED")
	for _, m := range r.Moves {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%v\n",
			m.Subject, m.Disposition, m.WaveID, m.Owner, m.SubjectFact, m.TargetFact,
			conflictsCell(m), m.FullyEvidenced)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	if len(r.Waves) > 0 {
		fmt.Fprintln(w, "waves:")
		for _, wave := range r.Waves {
			fmt.Fprintf(w, "  %s: %d move(s), %d fully evidenced; subject %s; target %s\n",
				wave.ID, wave.MovesTotal, wave.FullyEvidenced, factCountsLine(wave.BySubjectFact), factCountsLine(wave.ByTargetFact))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "unplanned conflicts (%d):\n", len(r.Unplanned))
	for _, id := range r.Unplanned {
		fmt.Fprintf(w, "  %s\n", id)
	}
	return nil
}

func factCountsLine(counts []FactCount) string {
	out := ""
	for i, c := range counts {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s %d", c.Value, c.Count)
	}
	return out
}

func conflictsCell(m MoveResult) string {
	present, stale := 0, 0
	for _, r := range m.Resolves {
		if r.Status == ResolvePresent {
			present++
		} else {
			stale++
		}
	}
	return fmt.Sprintf("resolved(present=%d,stale=%d) unclaimed=%d", present, stale, len(m.Unclaimed))
}
