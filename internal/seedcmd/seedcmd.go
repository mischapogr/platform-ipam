// Package seedcmd implements the `platform-ipam seed` process mode
// (docs/WORK_PLAN.md package N4, found by M9b1's review note;
// docs/DEPLOYMENT.md's process-mode table; docs/NETBOX_INTEGRATION.md
// section 6). It creates or verifies every NetBox custom field, choice set
// and tag the adapter relies on (internal/netbox.Client.Seed), idempotently
// and with that method's own conflict detection: an existing definition of
// another type, or with a different set of object types, is reported as a
// conflict and never rewritten.
//
// seed takes no subcommand and no flags: unlike onboard and adopt it has
// exactly one thing to do, so `platform-ipam seed` is the whole invocation
// -- any argument at all is a usage error. Like "client" and "onboard", it
// is dispatched in cmd/platform-ipam/main.go before
// storage.NewPostgresLedger is even constructed: it opens no database,
// loads no pools/identity configuration and builds no cloud observer
// (internal/config.Settings.Validate("seed") enforces that it needs nothing
// but the NetBox origin and token).
//
// It creates fields and tags ONLY -- never the development tenant, VRF,
// pools or sample inventory deploy/compose/seed-netbox.py's development
// bootstrap still owns.
package seedcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/netbox"
)

// Exit codes, matching internal/onboardcmd's and internal/adoptcmd's
// contract: ExitConflict (their ExitValidation, renamed here since seed has
// no operator-supplied data to validate -- the only thing it can find wrong
// is an existing NetBox object that disagrees with the catalogue) covers a
// custom field, choice set or tag that already exists with a different type
// or object-type set; ExitAdapter covers everything outside that: settings
// that fail Validate("seed"), an invalid NetBox adapter configuration, or a
// NetBox request that itself failed.
const (
	ExitOK       = 0
	ExitUsage    = 2
	ExitConflict = 3
	ExitAdapter  = 4
)

const helpText = "usage: platform-ipam seed\n\ncreates or verifies every NetBox custom field, choice set and tag this\nadapter relies on. Takes no subcommand and no flags.\n"

// Report is the one JSON document Main prints to stdout, on both the
// nothing-to-do/all-present path and the conflict path -- never on an
// adapter-class failure (settings, NetBox configuration or a request that
// itself failed), which writes nothing to stdout and reports the error to
// stderr instead, matching onboardcmd's own convention for that class of
// failure.
type Report struct {
	Objects []netbox.SeedResult `json:"objects"`
}

// Main runs one `platform-ipam seed` invocation and returns the process exit
// code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "platform-ipam seed: unexpected arguments %v\n\n%s", args, helpText)
		return ExitUsage
	}
	settings := config.Environment()
	if err := settings.Validate("seed"); err != nil {
		fmt.Fprintln(stderr, err)
		return ExitAdapter
	}
	inv, err := netbox.New(netbox.Config{BaseURL: settings.NetBoxURL, Token: settings.NetBoxToken})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitAdapter
	}
	results, err := inv.Seed(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "seed: %v\n", err)
		return ExitAdapter
	}
	data, err := json.Marshal(Report{Objects: results})
	if err != nil {
		fmt.Fprintf(stderr, "seed: encoding report: %v\n", err)
		return ExitAdapter
	}
	fmt.Fprintln(stdout, string(data))
	for _, r := range results {
		if r.Action == netbox.SeedConflict {
			return ExitConflict
		}
	}
	return ExitOK
}
