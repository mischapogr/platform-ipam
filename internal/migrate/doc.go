// Package migrate is the reviewed migration plan's schema and its decoder,
// described by docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md
// (work-plan package M3b1). It defines the shape of migration.yaml -- a
// document the customer writes and keeps, never this project -- and the
// allocation evidence file a command hands it, decodes either as the strict
// YAML-or-JSON the record describes, and refuses every structural problem
// ADR 0015 lists before any derivation ever sees the plan.
//
// This file (M3b1) does no derivation and no I/O of its own: DecodePlan and
// DecodeEvidence take an io.Reader and a name, exactly as the record's
// worked test list expects, and never open a file, call NetBox, call AWS or
// open the ledger themselves. It imports neither internal/netbox,
// internal/storage, internal/service, internal/cloud, internal/transport nor
// the AWS SDK (ADR 0015, "The plan has nowhere to write a CIDR": "the engine
// lives in a package that imports neither ... which makes most of it a
// property of the import graph rather than of vigilance" -- see
// TestImportsExcludeAdapterPackages). Unlike internal/assess, this package
// is not limited to the standard library: it imports
// go.yaml.in/yaml/v3, the repository's existing YAML dependency, directly --
// ADR 0015's own cut for this package says the decode is "the strict
// YAML-or-JSON decode through the repository's existing library", done here
// rather than pushed to the command layer the way internal/assess pushes it
// to internal/onboardcmd. See plan_decode.go's doc comment for why.
//
// Every field a reviewer types is carried verbatim; the schema has no field
// for a CIDR anywhere, on purpose (ADR 0015, "The plan has nowhere to write
// a CIDR"): a document with a "cidr" key at any depth is refused, never
// silently read (see plan_decode_property_test.go). Subject identity is ADR
// 0014's resource identity truncated before the CIDR -- account, region, VPC
// id -- exported as Subject.Identity() so a later derivation package (M3b2)
// can key its own maps by the same string this package uses for the
// duplicate-subject and depends_on refusals below.
//
// M3b2 (the derivation) and M3b3 (the command) are separate work-plan
// packages that will add their own files to this package; this file's
// symbols are named to leave that room (Plan, Wave, Move, Subject, Target,
// EvidenceFile, DecodePlan, DecodeEvidence, Merge, Validate, and the
// *StructuralError / *DecodeError error types) rather than to claim names a
// derivation or a report would want (Report, Derive, Evidence -- as opposed
// to EvidenceFile -- and so on).
package migrate
