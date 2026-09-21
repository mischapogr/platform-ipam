package transport

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

// A real signed token and discovery/JWKS exchange exercise the verification
// boundary; tenant claims intentionally disagree with the server mapping.
func TestOIDCIdentityBoundary(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "kid": "test-key", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	auth, err := NewAuth(context.Background(), config.Settings{AuthMode: "oidc", OIDCIssuer: issuer, OIDCAudience: "platform-ipam-api"}, []domain.Principal{{Subject: "workload", TenantID: "trusted-team"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		modify func(map[string]any)
		status int
	}{
		{"mapped subject ignores tenant claim", func(c map[string]any) {}, 0},
		{"wrong audience", func(c map[string]any) { c["aud"] = "frontend" }, 401},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://attacker.invalid" }, 401},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, 401},
		{"unmapped subject", func(c map[string]any) { c["sub"] = "stranger" }, 403},
		{"ID token rejected", func(c map[string]any) { c["token_use"] = "id" }, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{"iss": issuer, "aud": "platform-ipam-api", "sub": "workload", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "token_use": "access", "tenant_id": "attacker-team"}
			tc.modify(claims)
			header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
			body, _ := json.Marshal(claims)
			input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
			digest := sha256.Sum256([]byte(input))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "/v1/allocations", nil)
			request.Header.Set("Authorization", "Bearer "+input+"."+base64.RawURLEncoding.EncodeToString(signature))
			principal, err := auth.Authenticate(request)
			if tc.status == 0 {
				if err != nil || principal.TenantID != "trusted-team" {
					t.Fatalf("unexpected identity: %v, %v", principal, err)
				}
				return
			}
			apiErr, ok := err.(*domain.APIError)
			if !ok || apiErr.Status != tc.status {
				t.Fatalf("expected %d, got %v", tc.status, err)
			}
		})
	}
}

// TestOIDCRoleClaimIgnored proves the operator role (package G3b1) travels on
// the principal only from the server-side identity map NewAuth was
// constructed with, never from anything the caller supplies: a token whose
// claims carry "role":"operator" still authenticates as the configured
// principal's own (non-operator, empty) role. This mirrors
// TestOIDCIdentityBoundary's "mapped subject ignores tenant claim" case and
// its token-construction style, but asserts on Role/IsOperator() rather than
// TenantID, because it is structurally impossible for this to pass any other
// way: Authenticate's oidc branch reads only token.Subject (to look up the
// configured principal) and a TokenUse claim, and decodes nothing else from
// the token at all -- there is no code path by which a "role" claim could
// ever reach the returned Principal.
func TestOIDCRoleClaimIgnored(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "kid": "test-key", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	// The configured identity is an ordinary (non-operator) principal.
	auth, err := NewAuth(context.Background(), config.Settings{AuthMode: "oidc", OIDCIssuer: issuer, OIDCAudience: "platform-ipam-api"}, []domain.Principal{{Subject: "workload", TenantID: "trusted-team"}})
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"iss": issuer, "aud": "platform-ipam-api", "sub": "workload", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "token_use": "access", "role": "operator"}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/v1/allocations", nil)
	request.Header.Set("Authorization", "Bearer "+input+"."+base64.RawURLEncoding.EncodeToString(signature))
	principal, err := auth.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if principal.IsOperator() || principal.Role != "" {
		t.Fatalf("a token's role claim was honoured: %+v", principal)
	}
	if principal.TenantID != "trusted-team" {
		t.Fatalf("unexpected identity: %+v", principal)
	}
}

func TestAmbiguousCredentialsRejected(t *testing.T) {
	auth, err := NewAuth(context.Background(), config.Settings{Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer"}, []domain.Principal{{Subject: "developer", TenantID: "team"}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/allocations", nil)
	r.Header.Add("Authorization", "Bearer "+testToken)
	r.Header.Add("Authorization", "Bearer other")
	if _, err := auth.Authenticate(r); err == nil {
		t.Fatal("multiple credentials accepted")
	}
}

// -- package G3c: IPAM_LOCAL_EXTRA_CREDENTIALS -------------------------------

// TestExtraCredentialParsingRules is the pure, isolated half of every G3c
// parsing/format rule: each malformed shape fails on its own, independent of
// NewAuth's environment/mode gate.
func TestExtraCredentialParsingRules(t *testing.T) {
	identities := map[string]domain.Principal{
		"developer":    {Subject: "developer"},
		"ops-observer": {Subject: "ops-observer"},
		"auditor":      {Subject: "auditor"},
	}
	mainToken, mainSubject := testToken, "developer"
	valid := "ops-observer:ops-development-token-1234567890,auditor:audit-development-token-9876543210"

	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"unset means no extra credentials, i.e. today's behaviour", "", true},
		{"multiple valid pairs parse", valid, true},
		{"a pair with no colon is rejected", "ops-observer-no-colon", false},
		{"an empty subject is rejected", ":ops-development-token-1234567890", false},
		{"an empty token is rejected", "ops-observer:", false},
		{"leading whitespace in the token is rejected, not trimmed", "ops-observer: ops-development-token-123456789", false},
		{"trailing whitespace in the subject is rejected, not trimmed", "ops-observer :ops-development-token-1234567890", false},
		{"a token shorter than 24 characters is rejected", "ops-observer:too-short-token", false},
		{"a token equal to IPAM_LOCAL_TOKEN is rejected", "ops-observer:" + mainToken, false},
		{"a token duplicated across two pairs is rejected", "ops-observer:same-development-token-123456,auditor:same-development-token-123456", false},
		{"a subject equal to IPAM_LOCAL_SUBJECT is rejected", "developer:ops-development-token-1234567890", false},
		{"a subject duplicated across two pairs is rejected", "ops-observer:ops-development-token-1234567890,ops-observer:other-development-token-123456", false},
		{"a subject with no identity mapping is rejected", "stranger:ops-development-token-1234567890", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra, err := parseExtraCredentials(tc.raw, mainToken, mainSubject, identities)
			if tc.ok && err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected rejection, got %+v", extra)
			}
			// A start-up error is logged, so it may name a subject but never
			// carry anything that could be a token -- and a malformed entry
			// may be nothing but a token.
			if err != nil {
				for _, pair := range strings.Split(tc.raw, ",") {
					secret := strings.TrimSpace(pair[strings.IndexByte(pair, ':')+1:])
					if len(secret) >= 8 && strings.Contains(err.Error(), secret) {
						t.Fatalf("error text carries a token: %v", err)
					}
				}
			}
		})
	}
}

// TestExtraCredentialTokenSplitsOnFirstColonOnly proves a token may itself
// contain ':' -- only the pair's first colon separates subject from token.
func TestExtraCredentialTokenSplitsOnFirstColonOnly(t *testing.T) {
	identities := map[string]domain.Principal{"ops-observer": {Subject: "ops-observer"}}
	extra, err := parseExtraCredentials("ops-observer:part-one:part-two-of-a-token-value", testToken, "developer", identities)
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 1 || extra[0].subject != "ops-observer" || extra[0].token != "part-one:part-two-of-a-token-value" {
		t.Fatalf("unexpected parse result: %+v", extra)
	}
}

// TestSettingsFailClosed's stage-mode counterpart already blocks local auth
// mode entirely outside development; this proves NewAuth's own guard -- the
// second of package G3c's "two guards on purpose" -- independently refuses
// to start with IPAM_LOCAL_EXTRA_CREDENTIALS set outside development.
func TestNewAuthRefusesExtraCredentialsOutsideDevelopment(t *testing.T) {
	_, err := NewAuth(context.Background(), config.Settings{
		Environment: "stage", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer",
		LocalExtraCredentials: "ops-observer:ops-development-token-1234567890",
	}, []domain.Principal{{Subject: "developer", TenantID: "team"}, {Subject: "ops-observer", TenantID: "ops"}})
	if err == nil {
		t.Fatal("NewAuth accepted IPAM_LOCAL_EXTRA_CREDENTIALS outside development")
	}
}

// TestExtraCredentialsAuthenticate proves Authenticate resolves the caller's
// subject only from which configured credential matched -- never from
// anything the caller supplies -- for the primary token and every extra
// credential, and still rejects an unconfigured token.
func TestExtraCredentialsAuthenticate(t *testing.T) {
	opsToken := "ops-development-token-1234567890"
	auditToken := "audit-development-token-9876543210"
	auth, err := NewAuth(context.Background(), config.Settings{
		Environment: "development", AuthMode: "local", LocalToken: testToken, LocalSubject: "developer",
		LocalExtraCredentials: "ops-observer:" + opsToken + ",auditor:" + auditToken,
	}, []domain.Principal{
		{Subject: "developer", TenantID: "developer"},
		{Subject: "ops-observer", TenantID: "ops"},
		{Subject: "auditor", TenantID: "ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, token, wantTenant string
		wantStatus              int
	}{
		{"the primary token still authenticates the primary subject", testToken, "developer", 0},
		{"the first extra credential authenticates its own subject", opsToken, "ops", 0},
		{"the second extra credential authenticates its own subject", auditToken, "ops", 0},
		{"an unconfigured token is rejected", "not-a-configured-token-at-all-000000", "", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/allocations", nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			p, err := auth.Authenticate(r)
			if tc.wantStatus == 0 {
				if err != nil || p.TenantID != tc.wantTenant {
					t.Fatalf("unexpected identity: %+v, %v", p, err)
				}
				return
			}
			apiErr, ok := err.(*domain.APIError)
			if !ok || apiErr.Status != tc.wantStatus {
				t.Fatalf("expected %d, got %v", tc.wantStatus, err)
			}
		})
	}
}

// TestExtraCredentialsIgnoredAndWarnedInOIDCMode proves oidc mode ignores
// IPAM_LOCAL_EXTRA_CREDENTIALS entirely (NewAuth still succeeds with it set)
// and logs exactly one start-up warning naming the variable, without ever
// logging the token value itself.
func TestExtraCredentialsIgnoredAndWarnedInOIDCMode(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL

	extraToken := "ops-development-token-1234567890"
	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prior)

	_, err := NewAuth(context.Background(), config.Settings{
		AuthMode: "oidc", OIDCIssuer: issuer, OIDCAudience: "platform-ipam-api",
		LocalExtraCredentials: "ops-observer:" + extraToken,
	}, []domain.Principal{{Subject: "workload", TenantID: "trusted-team"}})
	if err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "IPAM_LOCAL_EXTRA_CREDENTIALS") {
		t.Fatalf("expected a start-up warning naming the ignored variable, got %q", logged)
	}
	if strings.Contains(logged, extraToken) {
		t.Fatal("the extra-credential token value must never be logged")
	}
}
