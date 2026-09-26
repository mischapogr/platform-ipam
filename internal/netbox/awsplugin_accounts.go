package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrAWSPluginAccountsUnavailable means the netbox-aws-vpc-plugin's
// aws-accounts endpoint answered 404: the plugin is not installed (or this
// NetBox does not expose it under the documented API root). It is a typed
// sentinel, not a generic HTTPError, so a caller can tell "the plugin is not
// there" apart from "NetBox is broken" (docs/WORK_PLAN.md package E2: an
// unreachable plugin must never be reported as "no drift").
//
// ProbeAWSPlugin (package N3, awsplugin.go) and ListAWSAccounts (package E2,
// this file) both wrap this same sentinel for a 404 on the accounts endpoint.
var ErrAWSPluginAccountsUnavailable = errors.New("netbox-aws-vpc-plugin aws-accounts endpoint is unavailable (is the plugin installed?)")

// awsAccountsPath is the plugin's REST endpoint for AWSAccount objects. The
// API root is /api/plugins/aws-vpc/, not the plugin's Python module name
// (netbox_aws_vpc_plugin) -- verified against a real install in package N2
// (docs/NETBOX_AWS_PLUGIN.md, "Installed and verified").
const awsAccountsPath = "/api/plugins/aws-vpc/aws-accounts/"

// AWSAccount is the subset of the plugin's AWSAccount object
// (https://github.com/dmaclaury/netbox-aws-vpc-plugin,
// netbox_aws_vpc_plugin/models/aws_account.py) that onboard drift (package
// E2, docs/WORK_PLAN.md) compares against configuration: account_id, name,
// status. ARN, description, tenant and comments are not read because drift
// does not report them.
type AWSAccount struct {
	ID        int
	AccountID string
	Name      string
	Status    string
}

// awsAccountRecord is the wire shape. AccountID is decoded leniently on
// purpose: NetBox represents an unset field as JSON null rather than omitting
// the key (internal/netbox/client.go's stringCF exists for the same reason on
// custom fields), and a hand-edited or out-of-band-created plugin object
// could carry an account_id that is empty, null, or not twelve digits. Package
// E2's contract is that such a row is reported as a finding, not a panic or a
// silently dropped object, so decoding must not fail the whole page over one
// bad value.
type awsAccountRecord struct {
	ID        int             `json:"id"`
	AccountID json.RawMessage `json:"account_id"`
	Name      string          `json:"name"`
	Status    choice          `json:"status"`
}

func decodeAWSAccount(raw json.RawMessage) (AWSAccount, error) {
	var rec awsAccountRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return AWSAccount{}, fmt.Errorf("decode plugin AWSAccount: %w", err)
	}
	return AWSAccount{ID: rec.ID, AccountID: rawAccountID(rec.AccountID), Name: rec.Name, Status: string(rec.Status)}, nil
}

// rawAccountID reads account_id as a string. A JSON null, an absent value, or
// any encoding that is not a bare JSON string all become "" rather than a
// decode error -- the caller (onboard drift) is the one that turns an empty
// or malformed account id into a reported finding.
func rawAccountID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// ListAWSAccounts pages through the plugin's AWSAccount collection using the
// same page helper Snapshot uses. It is read-only: no allocation or
// reconciliation decision may call this (docs/decisions/0009-..., "the plugin
// is a view, never the allocator's source of truth") -- only the operator
// drift check does (internal/onboardcmd/drift.go).
func (c *Client) ListAWSAccounts(ctx context.Context) ([]AWSAccount, error) {
	var out []AWSAccount
	err := c.page(ctx, awsAccountsPath, func(raw json.RawMessage) error {
		a, err := decodeAWSAccount(raw)
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %v", ErrAWSPluginAccountsUnavailable, err)
		}
		return nil, err
	}
	return out, nil
}
