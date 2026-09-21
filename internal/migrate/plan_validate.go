package migrate

import (
	"fmt"
	"reflect"
	"strings"
)

// This file is "merging several plan files" and "EVERY structural refusal
// the record lists" (ADR 0015, "M3b1, tier M"): a duplicate subject within
// or across files, two moves on one target key, a depends_on naming an
// undefined subject, a dependency cycle, an undefined wave, a replace move
// with no target, and a keep move whose target names more than a tenant and
// a key. The two decode-time refusals -- an unquoted numeric account id and
// any "cidr" key at any depth -- are not here: they fall out of
// DecodePlan's strict JSON decode in plan_decode.go, by construction (see
// that file's doc comment), so there is nothing for this file to add for
// them.
//
// Merge and Validate are deliberately two functions rather than one: a
// single decoded Plan (one file) and several merged ones are validated by
// the exact same code, because Validate only ever looks at the flat Moves
// and Waves slices a Plan carries, never at how many files produced them.
// "Within a file" and "across files" duplicate-subject and duplicate-target
// checks are therefore the same check, which is the point -- ADR 0015 never
// treats them as two different rules, only as two ways the same rule can be
// broken.

// RefusalKind names which of ADR 0015's structural refusals a
// *StructuralError reports.
type RefusalKind string

const (
	RefusalDuplicateSubject    RefusalKind = "duplicate_subject"
	RefusalDuplicateTargetKey  RefusalKind = "duplicate_target_key"
	RefusalUndefinedWave       RefusalKind = "undefined_wave"
	RefusalUndefinedDependency RefusalKind = "undefined_dependency"
	RefusalDependencyCycle     RefusalKind = "dependency_cycle"
	RefusalMissingTarget       RefusalKind = "missing_target"
	RefusalOverspecifiedTarget RefusalKind = "overspecified_target"
	// The three below are not in ADR 0015's list; they were added in review,
	// because each lets a document that does not say what its author meant
	// through as though it did. A disposition nobody defined would be derived
	// as no disposition at all; a subject missing a part names no VPC; and a
	// wave defined twice, differently, carries two owners or two approvals with
	// nothing to say which one a reader is looking at.
	RefusalUnknownDisposition RefusalKind = "unknown_disposition"
	RefusalIncompleteSubject  RefusalKind = "incomplete_subject"
	RefusalDuplicateWave      RefusalKind = "duplicate_wave"
)

// StructuralError is the one typed error every structural refusal in this
// file returns, discriminated by Kind rather than by a family of distinct
// Go types: a command turning any of them into exit 4 (ADR 0015, "Where it
// lives": "4 is no report: ... a structural refusal from the failure list
// below") needs exactly the same handling regardless of which refusal
// fired, and Kind plus File plus Key together already name what the lead's
// brief asks for -- "a typed error a command can turn into exit 4 naming
// file and key" -- without forcing a type switch on the caller. This is a
// decision the record's text does not make explicitly; see the report to
// the lead.
type StructuralError struct {
	// File is the source file of the move or wave the refusal is about (the
	// SECOND move/wave for a duplicate; see Detail for the first).
	File string
	// Key is the subject identity, the "tenant_id/allocation_key" pair, or
	// the wave id the refusal names.
	Key    string
	Kind   RefusalKind
	Detail string
}

func (e *StructuralError) Error() string {
	return fmt.Sprintf("%s: %s %q: %s", e.File, e.Kind, e.Key, e.Detail)
}

// Merge concatenates the Waves and Moves of every supplied Plan, in the
// order given, into one Plan. It performs no validation itself -- call
// Validate on the result (or on a single DecodePlan result; Merge is not a
// prerequisite for Validate) to apply every structural refusal. Merge takes
// Version and PlanID from the first supplied Plan, if any; a merged plan's
// own version and plan_id are otherwise undefined by ADR 0015's text, which
// only says "--plan is repeatable with a duplicate-subject refusal across
// files, so splitting a plan per product is supported" -- see the report to
// the lead.
func Merge(plans ...Plan) Plan {
	var merged Plan
	if len(plans) > 0 {
		merged.Version = plans[0].Version
		merged.PlanID = plans[0].PlanID
	}
	for _, p := range plans {
		merged.Waves = append(merged.Waves, p.Waves...)
		merged.Moves = append(merged.Moves, p.Moves...)
	}
	return merged
}

// Validate applies every structural refusal ADR 0015 lists for this
// package's cut, over p's Moves and Waves as a flat set (see the file
// comment for why "within a file" and "across files" are one check).
// Validate returns the FIRST refusal it finds, in a fixed order chosen for
// determinism rather than significance: duplicate subjects, then duplicate
// target keys, then -- per move, in input order -- undefined wave,
// undefined dependency, missing or over-specified target, and finally, once
// every dependency is known to name a real subject, a depends_on cycle.
// Every earlier check must pass before a later one can trust its inputs:
// the cycle check in particular assumes every depends_on entry already
// names a defined subject, which is exactly what the undefined-dependency
// check above it guarantees before Validate ever reaches it.
func Validate(p Plan) error {
	if err := validateDuplicateSubjects(p); err != nil {
		return err
	}
	if err := validateDuplicateTargetKeys(p); err != nil {
		return err
	}

	waveIDs := map[string]bool{}
	// Several files may each carry the definition of a wave they share, as long
	// as they say the same thing about it; two definitions that differ are two
	// owners or two approvals for one wave.
	waveSeen := map[string]Wave{}
	for _, w := range p.Waves {
		if first, seen := waveSeen[w.ID]; seen {
			a, b := first, w
			a.SourceFile, b.SourceFile = "", ""
			if !reflect.DeepEqual(a, b) {
				return &StructuralError{
					Kind: RefusalDuplicateWave, File: w.SourceFile, Key: w.ID,
					Detail: fmt.Sprintf("wave %q is defined differently in %s", w.ID, first.SourceFile),
				}
			}
			continue
		}
		waveIDs[w.ID] = true
		waveSeen[w.ID] = w
	}
	subjectDefined := map[string]bool{}
	for _, m := range p.Moves {
		subjectDefined[m.Subject.Identity()] = true
	}

	for _, m := range p.Moves {
		if m.Subject.AccountID == "" || m.Subject.Region == "" || m.Subject.VPCID == "" {
			return &StructuralError{
				Kind: RefusalIncompleteSubject, File: m.SourceFile, Key: m.Subject.Identity(),
				Detail: "a subject names an account, a region and a VPC id, all three",
			}
		}
		switch m.Disposition {
		case DispositionReplace, DispositionKeep, DispositionRetire, DispositionUndecided:
		default:
			return &StructuralError{
				Kind: RefusalUnknownDisposition, File: m.SourceFile, Key: m.Subject.Identity(),
				Detail: fmt.Sprintf("disposition %q is not one of replace, keep, retire, undecided", m.Disposition),
			}
		}
		if !waveIDs[m.Wave] {
			return &StructuralError{
				Kind: RefusalUndefinedWave, File: m.SourceFile, Key: m.Wave,
				Detail: fmt.Sprintf("move %s names wave %q, which no supplied document defines", m.Subject.Identity(), m.Wave),
			}
		}
		for _, dep := range m.DependsOn {
			if !subjectDefined[dep.Identity()] {
				return &StructuralError{
					Kind: RefusalUndefinedDependency, File: m.SourceFile, Key: dep.Identity(),
					Detail: fmt.Sprintf("move %s depends_on %s, which no move in the supplied documents defines", m.Subject.Identity(), dep.Identity()),
				}
			}
		}
		switch m.Disposition {
		case DispositionReplace:
			if m.Target == nil {
				return &StructuralError{
					Kind: RefusalMissingTarget, File: m.SourceFile, Key: m.Subject.Identity(),
					Detail: "a replace move must carry a target",
				}
			}
		case DispositionKeep:
			if m.Target != nil && m.Target.overspecified() {
				return &StructuralError{
					Kind: RefusalOverspecifiedTarget, File: m.SourceFile, Key: m.Subject.Identity(),
					Detail: "a keep move's target may name only tenant_id and allocation_key -- adoption pins the CIDR the VPC already has",
				}
			}
		}
	}

	if err := validateDependencyCycle(p); err != nil {
		return err
	}
	return nil
}

// validateDuplicateSubjects refuses a second move naming a subject identity
// an earlier move (in input order, whichever file it came from) already
// named (ADR 0015, "Failure modes": two moves for one VPC id is either a
// merge -- not attempted here -- or a mistake).
func validateDuplicateSubjects(p Plan) error {
	seen := map[string]Move{}
	for _, m := range p.Moves {
		id := m.Subject.Identity()
		if prior, ok := seen[id]; ok {
			return &StructuralError{
				Kind: RefusalDuplicateSubject, File: m.SourceFile, Key: id,
				Detail: fmt.Sprintf("subject %s already has a move in %s", id, prior.SourceFile),
			}
		}
		seen[id] = m
	}
	return nil
}

// validateDuplicateTargetKeys refuses a second move's target naming a
// (tenant_id, allocation_key) pair an earlier move's target already named
// (ADR 0015, "Failure modes": "A target key is reused. Two moves naming one
// tenant_id and allocation_key is a structural refusal"). A target naming
// neither field (the empty pair) is not compared -- there is nothing to
// collide on, and this package does not otherwise refuse a target with an
// empty tenant_id or allocation_key at the structural level; whether that
// is itself a defect is left to a later package's report, since ADR 0015
// does not list it as one of this cut's refusals.
func validateDuplicateTargetKeys(p Plan) error {
	seen := map[[2]string]Move{}
	for _, m := range p.Moves {
		if m.Target == nil {
			continue
		}
		k := m.Target.key()
		if k[0] == "" && k[1] == "" {
			continue
		}
		if prior, ok := seen[k]; ok {
			return &StructuralError{
				Kind: RefusalDuplicateTargetKey, File: m.SourceFile, Key: k[0] + "/" + k[1],
				Detail: fmt.Sprintf("target key %s/%s already used by the move for subject %s in %s", k[0], k[1], prior.Subject.Identity(), prior.SourceFile),
			}
		}
		seen[k] = m
	}
	return nil
}

// validateDependencyCycle refuses a cycle in the depends_on graph, whose
// nodes are subject identities and whose edges are each move's depends_on
// list (ADR 0015, "Failure modes": "a cycle in depends_on ... a structural
// refusal, exit 4, decided before any evidence is read"). Called only after
// validateDuplicateSubjects and Validate's own undefined-dependency loop
// have both passed, so every node this function visits is known to be
// unique and every edge is known to point at a defined subject; a standard
// three-colour DFS (white/gray/black) over p.Moves in input order, which is
// what makes the reported cycle deterministic when more than one exists.
func validateDependencyCycle(p Plan) error {
	type color int
	const (
		white color = iota
		gray
		black
	)

	adjacency := map[string][]string{}
	bySubject := map[string]Move{}
	for _, m := range p.Moves {
		id := m.Subject.Identity()
		bySubject[id] = m
		for _, dep := range m.DependsOn {
			adjacency[id] = append(adjacency[id], dep.Identity())
		}
	}

	colors := map[string]color{}
	var path []string
	var found *StructuralError

	var visit func(id string) bool
	visit = func(id string) bool {
		colors[id] = gray
		path = append(path, id)
		for _, next := range adjacency[id] {
			switch colors[next] {
			case white:
				if visit(next) {
					return true
				}
			case gray:
				start := 0
				for i, node := range path {
					if node == next {
						start = i
						break
					}
				}
				cycle := append(append([]string{}, path[start:]...), next)
				found = &StructuralError{
					Kind: RefusalDependencyCycle, File: bySubject[id].SourceFile, Key: next,
					Detail: fmt.Sprintf("depends_on cycle: %s", strings.Join(cycle, " -> ")),
				}
				return true
			}
		}
		path = path[:len(path)-1]
		colors[id] = black
		return false
	}

	for _, m := range p.Moves {
		id := m.Subject.Identity()
		if colors[id] == white {
			if visit(id) {
				return found
			}
		}
	}
	return nil
}
