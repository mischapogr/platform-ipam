package onboardcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
)

// This file implements `platform-ipam onboard drift` (docs/WORK_PLAN.md
// package E2, docs/decisions/0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md): a
// read-only operator check that compares the netbox-aws-vpc-plugin's
// AWSAccount objects against the domain's cloud_coverage, the domain's
// pools' eligible_accounts, and every identity's accounts.
//
// It writes nothing to NetBox, ever -- it calls netbox.Client.ListAWSAccounts
// (a GET-only path) and nothing else. It deliberately lives here, in the
// operator process mode, and not in the worker or internal/service: ADR 0009
// is that the plugin is a view, and no allocation or reconciliation decision
// may ever read it. drift is the one place in the codebase allowed to read
// the plugin, precisely because it decides nothing -- it only reports.

// Rule identifiers drift assigns to its findings. Stable across releases, so
// an operator or a script can key off them.
const (
	// DriftRuleUncoveredAccount: the account exists as an AWSAccount object in
	// the plugin but is not in this domain's cloud_coverage, so the worker
	// never observes networks in it -- they are invisible to reconciliation
	// and could be handed out again. Level is normally "error"; an account
	// whose plugin status is AWSAccountStatusChoices.STATUS_INACTIVE
	// ("INACTIVE" -- read from the plugin's source, see
	// awsAccountStatusInactive below) is reported at "info" instead, since a
	// deliberately retired account is not a reconciliation gap.
	DriftRuleUncoveredAccount = "uncovered-account"
	// DriftRuleUnknownToPlugin: the account is covered (and so observed) but
	// operators cannot see it in the NetBox account view. Level "warning".
	DriftRuleUnknownToPlugin = "unknown-to-plugin"
	// DriftRuleEligibleWithoutCoverage: a pool in this domain names the
	// account as eligible, but the domain's cloud_coverage has no cell for
	// it. internal/config.Validate already refuses to load a configuration
	// with this shape, so in the ordinary `onboard drift` path this rule
	// cannot fire -- it exists as defence in depth, for a configuration this
	// process loaded some other way than config.Load. Level "error".
	DriftRuleEligibleWithoutCoverage = "eligible-without-coverage"
	// DriftRuleIdentityWithoutCoverage: an identity mapping names the account,
	// but no overlap domain -- not just this one -- covers it. An identity
	// may legitimately target another domain, so this is computed over every
	// domain in the configuration, not just the one named by --domain. Level
	// "warning".
	DriftRuleIdentityWithoutCoverage = "identity-without-coverage"
	// DriftRuleInvalidPluginAccountID: the plugin returned an AWSAccount
	// object whose account_id is empty (including NetBox's JSON-null
	// representation of an unset field), or is not twelve digits. Such an
	// object cannot be compared against configuration at all, so it is
	// excluded from every other rule's P set and reported here instead of
	// crashing the command or being silently dropped. Level "warning".
	DriftRuleInvalidPluginAccountID = "invalid-plugin-account-id"
)

// Finding levels. Deliberately plain strings, not internal/onboard.Level:
// this is a different report shape for a different command, and reusing that
// type would couple this file to a package it otherwise has nothing to do
// with.
const (
	driftLevelError   = "error"
	driftLevelWarning = "warning"
	driftLevelInfo    = "info"
)

// awsAccountStatusInactive is AWSAccountStatusChoices.STATUS_INACTIVE from
// the plugin's own source
// (https://github.com/dmaclaury/netbox-aws-vpc-plugin/blob/main/netbox_aws_vpc_plugin/choices.py,
// fetched 2026-09-18):
//
//	class AWSAccountStatusChoices(ChoiceSet):
//	    key = "AWSAccount.status"
//	    STATUS_ACTIVE = "ACTIVE"
//	    STATUS_INACTIVE = "INACTIVE"
//	    STATUS_PENDING_ACTIVATION = "PENDING_ACTIVATION"
//
// Only STATUS_INACTIVE reads as "decommissioned" in the sense the work plan
// means: STATUS_PENDING_ACTIVATION is an account that is not yet usable, not
// one that has been retired, so it still raises DriftRuleUncoveredAccount at
// "error" like an unrecognized status would.
const awsAccountStatusInactive = "INACTIVE"

var driftAccountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// DriftFinding is one condition drift raised comparing the plugin's
// AWSAccount objects to configuration for one overlap domain.
type DriftFinding struct {
	Rule      string `json:"rule"`
	Level     string `json:"level"`
	AccountID string `json:"account_id"`
	Message   string `json:"message"`
}

// DriftCounts summarizes DriftReport.Findings so a caller need not recount
// them.
type DriftCounts struct {
	PluginAccounts   int `json:"plugin_accounts"`
	CoveredAccounts  int `json:"covered_accounts"`
	EligibleAccounts int `json:"eligible_accounts"`
	IdentityAccounts int `json:"identity_accounts"`
	Errors           int `json:"errors"`
	Warnings         int `json:"warnings"`
	Info             int `json:"info"`
}

// DriftReport is the JSON object `onboard drift` prints to stdout. Findings
// are sorted by rule, then account id, then message, so the output is
// deterministic across runs against the same inputs.
type DriftReport struct {
	Domain   string         `json:"domain"`
	Counts   DriftCounts    `json:"counts"`
	Findings []DriftFinding `json:"findings"`
}

// HasErrors reports whether any finding is at the "error" level -- the
// condition that makes `onboard drift` exit non-zero.
func (r DriftReport) HasErrors() bool {
	return r.Counts.Errors > 0
}

func runDrift(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "domain")
	set := newFlagSet("drift")
	domainID := set.String("domain", "", "overlap domain id (required)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 0 {
		return usageFail(stderr, usagef("drift: unexpected argument(s) %v", positional))
	}
	if *domainID == "" {
		return usageFail(stderr, usagef("drift: --domain is required"))
	}

	a, err := newAdapter()
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard drift: %v\n", err)
		return ExitAdapter
	}
	d, ok := resolveDomain(a.cfg, *domainID)
	if !ok {
		return usageFail(stderr, usagef("drift: unknown domain %q", *domainID))
	}

	// The one and only NetBox call this command ever makes: a read of the
	// plugin's AWSAccount collection. drift never calls Snapshot -- it has no
	// business with prefixes, addresses or ranges -- so nothing here can ever
	// touch allocation state, by construction rather than by care.
	accounts, err := a.inv.ListAWSAccounts(ctx)
	if err != nil {
		if errors.Is(err, netbox.ErrAWSPluginAccountsUnavailable) {
			fmt.Fprintf(stderr, "platform-ipam onboard drift: %v; see docs/NETBOX_AWS_PLUGIN.md and the optional deploy/compose/compose.netbox-plugin.yaml overlay\n", err)
		} else {
			fmt.Fprintf(stderr, "platform-ipam onboard drift: reading plugin AWS accounts: %v\n", err)
		}
		// An unreachable plugin (or a broken NetBox) is never reported as "no
		// drift": no report is printed, and the adapter exit code is used, not
		// ExitOK and not ExitValidation.
		return ExitAdapter
	}

	report := planDrift(a.cfg, d, accounts)
	if err := writeDriftReportJSON(stdout, report); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard drift: writing report: %v\n", err)
		return ExitAdapter
	}
	printDriftSummary(stderr, report)
	if report.HasErrors() {
		return ExitValidation
	}
	return ExitOK
}

func writeDriftReportJSON(stdout io.Writer, report DriftReport) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func printDriftSummary(stderr io.Writer, report DriftReport) {
	fmt.Fprintf(stderr, "platform-ipam onboard drift: domain %q: %d plugin account(s), %d error(s), %d warning(s), %d info\n",
		report.Domain, report.Counts.PluginAccounts, report.Counts.Errors, report.Counts.Warnings, report.Counts.Info)
}

// planDrift is the pure comparison: no I/O, no NetBox, config and the
// already-fetched plugin accounts are passed in. Kept separate from runDrift
// so DriftRuleEligibleWithoutCoverage -- unreachable through the real
// config.Load path, since internal/config.Validate already refuses that
// configuration shape -- can still be exercised directly with a
// hand-constructed domain.Config.
func planDrift(cfg domain.Config, d domain.Domain, accounts []netbox.AWSAccount) DriftReport {
	var findings []DriftFinding

	// P: plugin account ids that parse as twelve digits, mapped to their
	// status. Anything else is reported as DriftRuleInvalidPluginAccountID and
	// excluded from every other rule below -- comparing a null or malformed
	// account id against configuration would either crash or produce a
	// misleading finding.
	pluginAccounts := make(map[string]string, len(accounts))
	for _, acc := range accounts {
		if !driftAccountIDPattern.MatchString(acc.AccountID) {
			findings = append(findings, DriftFinding{
				Rule: DriftRuleInvalidPluginAccountID, Level: driftLevelWarning, AccountID: acc.AccountID,
				Message: fmt.Sprintf("plugin AWSAccount %d has account_id %q, which is not twelve digits", acc.ID, acc.AccountID),
			})
			continue
		}
		pluginAccounts[acc.AccountID] = acc.Status
	}

	// C: this domain's cloud_coverage account ids.
	covered := make(map[string]bool, len(d.CloudCoverage))
	for _, cell := range d.CloudCoverage {
		covered[cell.AccountID] = true
	}

	// C over every domain, for DriftRuleIdentityWithoutCoverage: an identity
	// account not covered by THIS domain may still be legitimately covered by
	// another one.
	coveredAnyDomain := make(map[string]bool)
	for _, other := range cfg.Domains {
		for _, cell := range other.CloudCoverage {
			coveredAnyDomain[cell.AccountID] = true
		}
	}

	// E: union of eligible_accounts of the pools that belong to this domain.
	eligible := make(map[string]bool)
	for _, p := range cfg.Pools {
		if p.DomainID != d.ID {
			continue
		}
		for _, acct := range p.EligibleAccounts {
			eligible[acct] = true
		}
	}

	// I: union of accounts over every identity, regardless of domain --
	// domain scoping happens when comparing against coveredAnyDomain below.
	identityAccounts := make(map[string]bool)
	for _, principal := range cfg.Identities {
		for _, acct := range principal.Accounts {
			identityAccounts[acct] = true
		}
	}

	for id, status := range pluginAccounts {
		if covered[id] {
			continue
		}
		level := driftLevelError
		message := fmt.Sprintf("account %s has an AWSAccount object in the NetBox plugin but is not in domain %q's cloud_coverage; networks in it are invisible to reconciliation", id, d.ID)
		if status == awsAccountStatusInactive {
			level = driftLevelInfo
			message = fmt.Sprintf("account %s has an AWSAccount object in the NetBox plugin marked %s and is not in domain %q's cloud_coverage; not reported as an error because the plugin marks it inactive", id, awsAccountStatusInactive, d.ID)
		}
		findings = append(findings, DriftFinding{Rule: DriftRuleUncoveredAccount, Level: level, AccountID: id, Message: message})
	}

	for id := range covered {
		if _, ok := pluginAccounts[id]; ok {
			continue
		}
		findings = append(findings, DriftFinding{Rule: DriftRuleUnknownToPlugin, Level: driftLevelWarning, AccountID: id,
			Message: fmt.Sprintf("account %s is in domain %q's cloud_coverage but has no AWSAccount object in the NetBox plugin", id, d.ID)})
	}

	for id := range eligible {
		if covered[id] {
			continue
		}
		findings = append(findings, DriftFinding{Rule: DriftRuleEligibleWithoutCoverage, Level: driftLevelError, AccountID: id,
			Message: fmt.Sprintf("account %s is eligible for a pool in domain %q but has no cloud_coverage cell", id, d.ID)})
	}

	for id := range identityAccounts {
		if covered[id] || coveredAnyDomain[id] {
			continue
		}
		findings = append(findings, DriftFinding{Rule: DriftRuleIdentityWithoutCoverage, Level: driftLevelWarning, AccountID: id,
			Message: fmt.Sprintf("account %s is targeted by an identity mapping but is not covered by any overlap domain (an identity may legitimately target a different domain than %q)", id, d.ID)})
	}

	sortDriftFindings(findings)

	counts := DriftCounts{PluginAccounts: len(accounts), CoveredAccounts: len(covered), EligibleAccounts: len(eligible), IdentityAccounts: len(identityAccounts)}
	for _, f := range findings {
		switch f.Level {
		case driftLevelError:
			counts.Errors++
		case driftLevelWarning:
			counts.Warnings++
		case driftLevelInfo:
			counts.Info++
		}
	}

	return DriftReport{Domain: d.ID, Counts: counts, Findings: findings}
}

func sortDriftFindings(findings []DriftFinding) {
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		return a.Message < b.Message
	})
}
