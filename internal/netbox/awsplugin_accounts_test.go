package netbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAWSAccountsPaginates(t *testing.T) {
	var s *httptest.Server
	s = localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != awsAccountsPath {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("offset") == "0" {
			w.Write(page([]any{map[string]any{"id": 1, "account_id": "111111111111", "name": "one",
				"status": map[string]any{"value": "ACTIVE", "label": "Active"}}}, s.URL+awsAccountsPath+"?limit=200&offset=200"))
			return
		}
		w.Write(page([]any{map[string]any{"id": 2, "account_id": "222222222222", "name": "two",
			"status": map[string]any{"value": "INACTIVE", "label": "Inactive"}}}, ""))
	}))
	defer s.Close()

	c, err := New(Config{BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := c.ListAWSAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAWSAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want exactly 2 across both pages", accounts)
	}
	if accounts[0].AccountID != "111111111111" || accounts[0].Status != "ACTIVE" {
		t.Fatalf("page 1 account = %+v", accounts[0])
	}
	if accounts[1].AccountID != "222222222222" || accounts[1].Status != "INACTIVE" {
		t.Fatalf("page 2 account = %+v", accounts[1])
	}
}

func TestListAWSAccountsPluginNotInstalledIsSentinel(t *testing.T) {
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer s.Close()

	c, err := New(Config{BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListAWSAccounts(context.Background())
	if !errors.Is(err, ErrAWSPluginAccountsUnavailable) {
		t.Fatalf("err = %v, want ErrAWSPluginAccountsUnavailable", err)
	}
}

func TestListAWSAccountsNetBoxServerErrorIsNotTheNotInstalledSentinel(t *testing.T) {
	// A broken NetBox (500) must be distinguishable from "the plugin is not
	// installed" (404): the operator-facing message differs, and this is what
	// lets onboard drift point at docs/NETBOX_AWS_PLUGIN.md only in the
	// not-installed case.
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer s.Close()

	c, err := New(Config{BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListAWSAccounts(context.Background())
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if errors.Is(err, ErrAWSPluginAccountsUnavailable) {
		t.Fatalf("a 500 must not be reported as ErrAWSPluginAccountsUnavailable: %v", err)
	}
}

func TestListAWSAccountsDecodesNullAndMalformedAccountIDWithoutCrashing(t *testing.T) {
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(page([]any{
			map[string]any{"id": 1, "account_id": nil, "name": "nulled", "status": "active"},
			map[string]any{"id": 2, "account_id": "12345", "name": "tooshort", "status": "active"},
			map[string]any{"id": 3, "account_id": "123456789012", "name": "valid", "status": "active"},
		}, ""))
	}))
	defer s.Close()

	c, err := New(Config{BaseURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := c.ListAWSAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAWSAccounts must not fail on a null or malformed account_id: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %+v, want exactly 3", accounts)
	}
	if accounts[0].AccountID != "" {
		t.Fatalf("null account_id decoded as %q, want empty string, not the string \"<nil>\"", accounts[0].AccountID)
	}
	if accounts[1].AccountID != "12345" {
		t.Fatalf("malformed account_id decoded as %q, want the raw value preserved for the caller to report", accounts[1].AccountID)
	}
	if accounts[2].AccountID != "123456789012" {
		t.Fatalf("valid account_id decoded as %q", accounts[2].AccountID)
	}
	// The bare-string status form ("active") must decode the same as the
	// {"value":...} object form used elsewhere in this test file: NetBox 4.x
	// renders choice fields as objects, but the `choice` type also accepts a
	// bare string (see client.go).
	if accounts[2].Status != "active" {
		t.Fatalf("Status = %q, want %q", accounts[2].Status, "active")
	}
}
