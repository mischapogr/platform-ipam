package onboardcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
)

// This file tests `platform-ipam onboard remove` (docs/WORK_PLAN.md package
// M9b4, ADR 0016). buildRemoveReport is the pure decision at the command's
// centre -- it takes what NetBox already answered and what the evidence
// collection already decoded to, and never calls NetBox or touches a file
// itself -- so almost every case below calls it directly with hand-built
// values, the same style internal/assess's own tests use for Input and
// Options. Account ids are the repository's synthetic convention: the
// all-zero account or a repdigit (docs/WORK_PLAN.md, and
// internal/onboardcmd/assess_test.go's own TestAssessFixturesUseOnlySyntheticAccountIDs).

func removeStr(s string) *string { return &s }

// baseContributor is the one contributor every test starts from: known,
// fresh, non-degenerate, and absent from the base (empty) records set.
func baseContributor() domain.Contributor {
	return domain.Contributor{
		Identity:       "aws:111111111111:eu-central-1:vpc-aaaa:10.0.1.0/24",
		AccountID:      "111111111111",
		Region:         "eu-central-1",
		Type:           "vpc",
		ResourceID:     "vpc-aaaa",
		ObservedAt:     removeStr("2026-09-20T10:00:00Z"),
		FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
		SourceFile: "networks.csv", SourceRow: 2,
	}
}

// baseDetail is a removable prefix: owned by nobody, tagged imported, one
// known fresh contributor, and a description that is exactly what
// expectedContributorDescription computes from that one contributor --
// "account 111111111111 | resource id vpc-aaaa".
func baseDetail() netbox.OccupancyDetail {
	c := baseContributor()
	return netbox.OccupancyDetail{
		ID: "42", CIDR: "10.0.1.0/24", Imported: true,
		Contributors: []domain.Contributor{c},
		Description:  expectedContributorDescription([]domain.Contributor{c}),
	}
}

func baseRun() *assess.RunRecord {
	return &assess.RunRecord{
		StartedAt: "2026-09-21T00:00:00Z", FinishedAt: "2026-09-21T00:10:00Z", SourceFile: "run.json",
		Attempts: []assess.RunAttempt{{AccountID: "111111111111", Region: "eu-central-1", Outcome: assess.AttemptSucceeded}},
	}
}

func refusalCodes(report removeReport) []string {
	var out []string
	for _, r := range report.Refusals {
		out = append(out, r.Code)
	}
	return out
}

func hasRefusal(report removeReport, code string) bool {
	for _, r := range report.Refusals {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestBuildRemoveReportTheHappyPathIsRemovable(t *testing.T) {
	report := buildRemoveReport("connected", baseDetail(), nil, baseRun(), assess.Report{}, false, "")
	if !report.Removable {
		t.Fatalf("expected removable, refusals: %v", refusalCodes(report))
	}
	if len(report.Contributors) != 1 || !report.Contributors[0].ObservedAbsent {
		t.Fatalf("expected the one contributor to be observed absent: %#v", report.Contributors)
	}
	if len(report.Coverage) != 1 || !report.Coverage[0].Complete || report.Coverage[0].Outcome != "succeeded" {
		t.Fatalf("unexpected coverage: %#v", report.Coverage)
	}
}

func TestBuildRemoveReportOutrightRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(netbox.OccupancyDetail) netbox.OccupancyDetail
		code   string
	}{
		{"owned", func(d netbox.OccupancyDetail) netbox.OccupancyDetail { d.Owned = true; return d }, "prefix_owned"},
		{"not imported", func(d netbox.OccupancyDetail) netbox.OccupancyDetail { d.Imported = false; return d }, "not_imported"},
		{"pool conflict", func(d netbox.OccupancyDetail) netbox.OccupancyDetail { d.PoolConflict = "contains pool p1"; return d }, "pool_prefix"},
		{"empty contributor list", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Contributors = []domain.Contributor{}
			return d
		}, "empty_contributor_list"},
		{"description edited", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Description = "an operator wrote this"
			return d
		}, "description_edited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := buildRemoveReport("connected", tt.mutate(baseDetail()), nil, baseRun(), assess.Report{}, false, "")
			if report.Removable {
				t.Fatalf("expected NOT removable")
			}
			if !hasRefusal(report, tt.code) {
				t.Fatalf("expected refusal %q, got %v", tt.code, refusalCodes(report))
			}
		})
	}
}

func TestBuildRemoveReportDescriptionMatchesForAnUneditedMultiContributorPrefix(t *testing.T) {
	contributors := []domain.Contributor{baseContributor(), {
		Identity:       "aws:222222222222:eu-central-1:vpc-bbbb:10.0.1.0/24",
		AccountID:      "222222222222",
		Region:         "eu-central-1",
		Type:           "vpc",
		ResourceID:     "vpc-bbbb",
		ObservedAt:     removeStr("2026-09-20T10:00:00Z"),
		FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
		SourceFile: "networks.csv", SourceRow: 3,
	}}
	detail := netbox.OccupancyDetail{
		ID: "42", CIDR: "10.0.1.0/24", Imported: true, Contributors: contributors,
		Description: expectedContributorDescription(contributors),
	}
	run := &assess.RunRecord{
		FinishedAt: "2026-09-21T00:10:00Z",
		Attempts: []assess.RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: assess.AttemptSucceeded},
			{AccountID: "222222222222", Region: "eu-central-1", Outcome: assess.AttemptSucceeded},
		},
	}
	report := buildRemoveReport("connected", detail, nil, run, assess.Report{}, false, "")
	if hasRefusal(report, "description_edited") {
		t.Fatalf("an honest reconstruction of an unedited multi-contributor description must not be refused: %v", refusalCodes(report))
	}
}

func TestBuildRemoveReportRule1EveryContributorKnown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(netbox.OccupancyDetail) netbox.OccupancyDetail
		code   string
	}{
		{"unreadable", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Unreadable, d.Contributors = true, nil
			return d
		}, "contributor_unreadable"},
		{"no list at all", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Contributors = nil
			d.Description = ""
			return d
		}, "contributor_unknown"},
		{"reconstructed", func(d netbox.OccupancyDetail) netbox.OccupancyDetail { d.Reconstructed = true; return d }, "contributor_reconstructed"},
		{"null observed_at", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Contributors[0].ObservedAt = nil
			return d
		}, "contributor_stale_source"},
		{"degenerate identity", func(d netbox.OccupancyDetail) netbox.OccupancyDetail {
			d.Contributors[0].ResourceID = ""
			return d
		}, "contributor_degenerate_identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Deep-copy the one contributor so one test's mutation cannot
			// leak into another's slice backing array.
			base := baseDetail()
			cp := append([]domain.Contributor{}, base.Contributors...)
			base.Contributors = cp
			d := tt.mutate(base)
			report := buildRemoveReport("connected", d, nil, baseRun(), assess.Report{}, false, "")
			if report.Removable {
				t.Fatalf("expected NOT removable")
			}
			if !hasRefusal(report, tt.code) {
				t.Fatalf("expected refusal %q, got %v", tt.code, refusalCodes(report))
			}
		})
	}
}

func TestBuildRemoveReportRule2CoverageIncomplete(t *testing.T) {
	ar := assess.AccountRegion{AccountID: "111111111111", Region: "eu-central-1"}
	tests := []struct {
		name string
		cov  assess.Coverage
		run  *assess.RunRecord
	}{
		// run is non-nil and itself says "succeeded" for the matching
		// account/region: RunMissing must still refuse on its own, not rely
		// on a nil run.json to fall through to "not listed in the run
		// record" by coincidence -- that would leave the RunMissing check
		// able to be deleted without any test noticing.
		{"run.json missing", assess.Coverage{RunMissing: true}, baseRun()},
		{"accounts.json missing", assess.Coverage{AccountsMissing: true}, baseRun()},
		{"failed", assess.Coverage{Failed: []assess.FailedEntry{{AccountID: ar.AccountID, Region: ar.Region, Error: "describe: AccessDenied"}}}, baseRun()},
		{"partial", assess.Coverage{Partial: []assess.AccountRegion{ar}}, baseRun()},
		{"not attempted", assess.Coverage{NotAttempted: []assess.AccountRegion{ar}}, baseRun()},
		{"row count short", assess.Coverage{RowCountShort: []assess.RowCountMismatchEntry{{AccountID: ar.AccountID, Region: ar.Region, Recorded: 5, Present: 3}}}, baseRun()},
		{"not listed in the run at all", assess.Coverage{}, &assess.RunRecord{FinishedAt: "2026-09-21T00:10:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := buildRemoveReport("connected", baseDetail(), nil, tt.run, assess.Report{Coverage: tt.cov}, false, "")
			if report.Removable {
				t.Fatalf("expected NOT removable")
			}
			if !hasRefusal(report, "coverage_incomplete") {
				t.Fatalf("expected coverage_incomplete, got %v", refusalCodes(report))
			}
		})
	}
}

func TestBuildRemoveReportRule3ObservedAbsent(t *testing.T) {
	t.Run("still present in the collection", func(t *testing.T) {
		records := []assess.ResourceRecord{{AccountID: "111111111111", Region: "eu-central-1", ResourceID: "vpc-aaaa", Type: assess.TypeVPC}}
		report := buildRemoveReport("connected", baseDetail(), records, baseRun(), assess.Report{}, false, "")
		if report.Removable {
			t.Fatalf("expected NOT removable")
		}
		if !hasRefusal(report, "not_observed_absent") {
			t.Fatalf("expected not_observed_absent, got %v", refusalCodes(report))
		}
	})
	t.Run("observed_at not older than the collection's finished_at", func(t *testing.T) {
		run := &assess.RunRecord{FinishedAt: "2026-09-20T00:00:00Z"} // before baseContributor's ObservedAt
		report := buildRemoveReport("connected", baseDetail(), nil, run, assess.Report{}, false, "")
		if report.Removable {
			t.Fatalf("expected NOT removable")
		}
		if !hasRefusal(report, "not_observed_absent") {
			t.Fatalf("expected not_observed_absent, got %v", refusalCodes(report))
		}
	})
	t.Run("absent in a different account is not evidence", func(t *testing.T) {
		records := []assess.ResourceRecord{{AccountID: "999999999999", Region: "eu-central-1", ResourceID: "vpc-aaaa", Type: assess.TypeVPC}}
		report := buildRemoveReport("connected", baseDetail(), records, baseRun(), assess.Report{}, false, "")
		if !report.Removable {
			t.Fatalf("expected removable, refusals: %v", refusalCodes(report))
		}
	})
}

func TestBuildRemoveReportRule4ReviewedApply(t *testing.T) {
	t.Run("id mismatch refuses", func(t *testing.T) {
		report := buildRemoveReport("connected", baseDetail(), nil, baseRun(), assess.Report{}, true, "99")
		if report.Removable {
			t.Fatalf("expected NOT removable")
		}
		if !hasRefusal(report, "netbox_id_mismatch") {
			t.Fatalf("expected netbox_id_mismatch, got %v", refusalCodes(report))
		}
	})
	t.Run("matching id applies cleanly", func(t *testing.T) {
		report := buildRemoveReport("connected", baseDetail(), nil, baseRun(), assess.Report{}, true, "42")
		if !report.Removable {
			t.Fatalf("expected removable, refusals: %v", refusalCodes(report))
		}
	})
}

// TestBuildRemoveReportEveryRuleReportedTogether is the M9b4 dry run's own
// point: it does not stop at the first refusal. An owned, unimported,
// contributor-less prefix accumulates every one of those refusals in a
// single call, not just the first.
func TestBuildRemoveReportEveryRuleReportedTogether(t *testing.T) {
	d := netbox.OccupancyDetail{ID: "42", CIDR: "10.0.1.0/24", Owned: true, Imported: false, Contributors: []domain.Contributor{}}
	report := buildRemoveReport("connected", d, nil, nil, assess.Report{}, false, "")
	for _, code := range []string{"prefix_owned", "not_imported", "empty_contributor_list"} {
		if !hasRefusal(report, code) {
			t.Fatalf("expected refusal %q among %v", code, refusalCodes(report))
		}
	}
}

// --- runRemove: usage and flag parsing (no NetBox needed) ---

func TestRunRemoveRequiresExactlyOneCIDR(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRemove(context.Background(), []string{"--domain", "connected"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "exactly one CIDR") {
		t.Fatalf("stderr does not name the usage problem: %s", stderr.String())
	}
}

func TestRunRemoveRequiresDomain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRemove(context.Background(), []string{"10.0.1.0/24"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage; stderr: %s", code, stderr.String())
	}
}

func TestRunRemoveApplyRequiresID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRemove(context.Background(), []string{"10.0.1.0/24", "--domain", "connected", "--apply"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--id") {
		t.Fatalf("stderr does not mention --id: %s", stderr.String())
	}
}

func TestRunRemoveRequiresAnEvidenceCollection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRemove(context.Background(), []string{"10.0.1.0/24", "--domain", "connected"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "evidence collection") {
		t.Fatalf("stderr does not name the missing evidence collection: %s", stderr.String())
	}
}
