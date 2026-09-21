package onboard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// TestPlanByteIdenticalForExistingFixtures is the head property M1b1 must
// hold (docs/WORK_PLAN.md M1b1, ADR 0014 "The input, and what must be added
// to produce it"): an old networks file, with none of the new columns
// (association_id, observed_at), must still parse and plan exactly as
// before. This test was written and captured (UPDATE_GOLDEN=1) against the
// package BEFORE NetworkRow gained AssociationID, ObservedAt, ObservedAtParsed
// and SourceFile, and is run again unchanged afterwards: an unchanged golden
// file is the evidence that Plan's Report -- not just the reader -- produced
// byte-identical JSON for every existing fixture.
//
// It deliberately reads through ReadTable/ReadText, the real entry points a
// caller uses, rather than constructing a Table literal: a literal would
// trivially have every new field at its zero value regardless of what the
// reader actually does, and would not catch a change to buildNetworkRows.
func TestPlanByteIdenticalForExistingFixtures(t *testing.T) {
	cfg := testCfg()
	snap := domain.InventorySnapshot{Complete: true}

	fixtures := []string{"networks.csv", "networks_semicolon.csv", "confluence_paste.txt"}
	var out bytes.Buffer
	for _, name := range fixtures {
		f, size := openTestdata(t, name)
		table, err := ReadTable(name, f, size, ReadOptions{})
		if err != nil {
			t.Fatalf("ReadTable(%s): %v", name, err)
		}
		report := Plan(table, cfg, testDomainID, snap)

		record := struct {
			Fixture string
			Report  Report
		}{Fixture: name, Report: report}
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			t.Fatalf("marshaling report for %s: %v", name, err)
		}
		out.Write(data)
		out.WriteByte('\n')
	}

	goldenPath := filepath.Join("testdata", "plan_byte_identity.golden")
	golden := readOrUpdateGoldenPlan(t, goldenPath, out.Bytes())
	if !bytes.Equal(out.Bytes(), golden) {
		t.Errorf("Plan output for an existing fixture changed byte-for-byte.\nGot:\n%s\n\nWant:\n%s",
			out.String(), string(golden))
	}
}

// readOrUpdateGoldenPlan mirrors render_test.go's readOrUpdateGolden. It is
// duplicated (not shared) so this file has no compile-time dependency on
// render_test.go's test-only helper staying exported the way it is today.
func readOrUpdateGoldenPlan(t *testing.T, path string, data []byte) []byte {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write golden file: %v", err)
		}
		return data
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run with UPDATE_GOLDEN=1 to create it): %v", err)
	}
	return content
}
