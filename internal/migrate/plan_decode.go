package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	yaml "go.yaml.in/yaml/v3"
)

// This file is the strict YAML-or-JSON decoder ADR 0015 assigns to this
// package's M3b1 cut ("the strict YAML-or-JSON decode through the
// repository's existing library with unknown fields refused"). Unlike
// internal/assess, which stays standard-library-only and pushes the
// YAML-to-JSON conversion into the command layer
// (internal/onboardcmd.readYAMLOrJSONFile) because assess's own import-graph
// test forbids it any non-stdlib import, this package is not held to that
// same test: ADR 0015 never says internal/migrate must be stdlib-only, only
// that it "imports neither internal/netbox, internal/storage,
// internal/service, internal/cloud nor the AWS SDK" (see "The plan has
// nowhere to write a CIDR"), a narrower list that does not exclude
// go.yaml.in/yaml/v3 -- already a direct dependency in go.mod, so decoding
// YAML here adds nothing to go.mod or go.sum. This is the decision the
// record leaves to this package to make; see the report to the lead.
//
// The mechanism is the same one internal/onboardcmd already uses and for
// the same two reasons: unmarshal into a generic value with the YAML
// library (every valid JSON document is already valid YAML 1.2, so this one
// path reads both formats), re-encode as JSON, then decode that JSON into
// the typed schema with encoding/json's DisallowUnknownFields. Doing the
// strict-schema half through encoding/json rather than through yaml.v3's
// own strict mode means a JSON *number* in a string field (the YAML-integer
// trap: an unquoted account id, in particular one with a leading zero) is
// refused by ordinary Go type-checking rather than by a decoder-specific
// setting -- see TestDecodePlanRejectsUnquotedAccountID and
// TestDecodePlanAcceptsQuotedAccountID.

// DecodeError wraps a decode failure with the name of the file it came
// from, so a command turning it into exit 4 can name the file without
// re-deriving it from context. It is the "typed error a command can turn
// into exit 4 naming file and key" this package's decode-time refusals
// produce; Unwrap exposes the underlying error (an *json.UnmarshalTypeError
// for the unquoted-account-id case, a plain error naming the unknown field
// for an unrecognised key, or a YAML syntax error) for a caller that wants
// to inspect it further.
type DecodeError struct {
	File string
	Err  error
}

func (e *DecodeError) Error() string { return fmt.Sprintf("%s: %v", e.File, e.Err) }
func (e *DecodeError) Unwrap() error { return e.Err }

// yamlOrJSONToStrictJSON reads r fully, decodes it as YAML (which also
// accepts JSON, since every JSON document is valid YAML 1.2) into a generic
// value, and re-encodes that value as JSON -- the same conversion
// internal/onboardcmd.readYAMLOrJSONFile performs, reused here rather than
// shared as an exported helper across the two packages, since sharing it
// would require this package to depend on internal/onboardcmd or vice
// versa, and neither dependency is one this package's import-graph
// properties should acquire.
func yamlOrJSONToStrictJSON(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}
	var generic interface{}
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("not valid YAML or JSON: %w", err)
	}
	// An empty or all-comment YAML document unmarshals to a bare "null" with
	// no error, and encoding/json silently leaves a struct target unchanged
	// (zero-valued) when the JSON body is the literal null -- so without this
	// check an empty file would decode as a valid, empty Plan or EvidenceFile
	// rather than being refused. Nothing in ADR 0015's text names this case;
	// it is this package's own decision, made the same way DecodePlan already
	// treats every other "the bytes are not this schema" problem: refused,
	// not guessed at. See the report to the lead.
	if generic == nil {
		return nil, fmt.Errorf("document is empty (decodes to YAML/JSON null)")
	}
	converted, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("re-encoding as JSON: %w", err)
	}
	return converted, nil
}

// DecodePlan decodes r as one migration.yaml (or .json) document: the
// strict YAML-or-JSON path above, then a strict JSON decode into Plan with
// unknown fields refused -- which is also what refuses a "cidr" key at any
// depth (this package's schema has no field named cidr anywhere, so any
// occurrence, at any nesting, is an unknown field to some struct in the
// decode) and what refuses a target's over-specification is NOT decided
// here (that is Validate's job, after the whole document is a Plan value);
// this function only decides whether the bytes ARE the schema. name is
// stamped onto every decoded Wave and Move as SourceFile, and into the
// returned error if decoding fails, so a caller never has to re-attach it
// itself. DecodePlan performs no I/O beyond reading r.
func DecodePlan(r io.Reader, name string) (Plan, error) {
	jsonBytes, err := yamlOrJSONToStrictJSON(r)
	if err != nil {
		return Plan{}, &DecodeError{File: name, Err: err}
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Plan{}, &DecodeError{File: name, Err: err}
	}
	if extra, err := dec.Token(); err != io.EOF {
		return Plan{}, &DecodeError{File: name, Err: fmt.Errorf("unexpected trailing content after the first document: %v", extra)}
	}
	for i := range p.Waves {
		p.Waves[i].SourceFile = name
	}
	for i := range p.Moves {
		p.Moves[i].SourceFile = name
	}
	return p, nil
}

// DecodeEvidence decodes r as one allocation evidence file: the same
// strict YAML-or-JSON path DecodePlan uses (an evidence export is written
// as JSON by the CLI that produces it, per ADR 0015, but nothing in this
// package requires that -- accepting YAML here costs nothing and keeps one
// code path for every reviewed or authenticated input this package reads).
// name is stamped onto the returned EvidenceFile as SourceFile.
// DecodeEvidence performs no I/O beyond reading r.
func DecodeEvidence(r io.Reader, name string) (EvidenceFile, error) {
	jsonBytes, err := yamlOrJSONToStrictJSON(r)
	if err != nil {
		return EvidenceFile{}, &DecodeError{File: name, Err: err}
	}
	var ev EvidenceFile
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return EvidenceFile{}, &DecodeError{File: name, Err: err}
	}
	if extra, err := dec.Token(); err != io.EOF {
		return EvidenceFile{}, &DecodeError{File: name, Err: fmt.Errorf("unexpected trailing content after the first document: %v", extra)}
	}
	ev.SourceFile = name
	return ev, nil
}
