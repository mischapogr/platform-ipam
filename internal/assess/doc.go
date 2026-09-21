// Package assess is the pure, offline resource-aware overlap-assessment
// engine described by docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md
// (work-plan package M1b2). It answers one question over data the caller
// already holds in memory: which observed AWS VPC CIDR associations conflict
// with which others, why, and for which part of the estate no complete
// statement can be made.
//
// The package does no I/O beyond decoding the byte slices or io.Readers its
// callers hand it (DecodeMatrix, DecodeOwnership, DecodeFixed,
// DecodeDecisions): it never opens a file itself, never calls NetBox or AWS,
// never reads an environment variable, and never calls time.Now -- the
// caller supplies every instant, including the optional archival --stamp,
// through Options.Stamp. It imports nothing beyond the standard library (see
// TestImportsAreStandardLibraryOnly), which is what makes "it never calls
// NetBox or AWS" a property of the import graph rather than of care.
//
// It shares nothing with internal/onboard.Plan on purpose. Plan collapses
// rows that resolve to the same CIDR into one write, because a duplicate
// (vrf, cidr) is fatal to the platform-managed inventory snapshot. This
// package must never collapse two different VPCs' ranges into one fact --
// that is exactly the conflict a reviewer needs to see -- so it reads
// internal/onboard's row shape only by convention (Input's fields mirror
// NetworkRow's, so a caller mapping one onto the other is mechanical) and
// otherwise shares no code and no types with it.
//
// Entry points: Assess builds a Report from an Input and Options. The two
// head properties every test in this package answers to are named in the
// ADR: every reported Conflict is traceable to the exact two input records,
// by file and row, that produced it (see Side.SourceFile/SourceRow and
// TestConflictsAreTraceableToTheirSourceRecords); and a Report produced over
// incomplete coverage never claims completeness (see
// TestIncompleteCoverageNeverClaimsCompleteness and the forbidden-phrase
// scan in report_test.go).
package assess
