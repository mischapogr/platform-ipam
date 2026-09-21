package config

import "testing"

// -- package H5: adopt (and, if the same quirk holds, worker) must not
// demand settings they never use --------------------------------------------
//
// TestSettingsValidateAPIAndMigrateUnchanged pins every existing
// Settings.Validate rule, one case per rule, for the two modes package H5
// must leave byte for byte: "api" and "migrate". It is written and run
// against the UNMODIFIED code first (recorded in the H5 report), before any
// change, and must still pass unmodified afterwards -- proving the
// mode-aware rewrite changed nothing observable for these two modes.
func TestSettingsValidateAPIAndMigrateUnchanged(t *testing.T) {
	validStageAPI := Settings{
		Environment: "stage", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
		AuthMode: "oidc", OIDCIssuer: "https://idp.example.org", OIDCAudience: "platform-ipam-stage",
		AWSMode: "live", NetBoxURL: "https://netbox.stage.example.org", NetBoxToken: "netbox-token",
	}
	validProdAPI := validStageAPI
	validProdAPI.Environment = "prod"
	validProdAPI.NetBoxURL = "https://netbox.prod.example.org"

	validDevAPI := Settings{
		Environment: "development", DatabaseURL: "postgres://localhost/ipam",
		AuthMode: "local", LocalToken: "local-development-token-123456", LocalSubject: "developer",
		AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
		NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
	}

	withField := func(base Settings, set func(*Settings)) Settings {
		s := base
		set(&s)
		return s
	}

	cases := []struct {
		name    string
		mode    string
		s       Settings
		wantErr bool
	}{
		{"stage api: valid baseline", "api", validStageAPI, false},
		{"prod api: valid baseline", "api", validProdAPI, false},
		{"dev api: valid baseline (local auth)", "api", validDevAPI, false},

		{"stage api: missing OIDC issuer", "api", withField(validStageAPI, func(s *Settings) { s.OIDCIssuer = "" }), true},
		{"stage api: missing OIDC audience", "api", withField(validStageAPI, func(s *Settings) { s.OIDCAudience = "" }), true},
		{"stage api: OIDC issuer not HTTPS", "api", withField(validStageAPI, func(s *Settings) { s.OIDCIssuer = "http://idp.example.org" }), true},
		{"stage api: AWSMode fake instead of live", "api", withField(validStageAPI, func(s *Settings) { s.AWSMode = "fake"; s.FakeCloudFile = "fixtures/cloud.json" }), true},
		{"stage api: AuthMode local instead of oidc", "api", withField(validStageAPI, func(s *Settings) { s.AuthMode = "local"; s.LocalToken = "local-development-token-123456" }), true},
		{"stage api: sslmode not verify-full", "api", withField(validStageAPI, func(s *Settings) { s.DatabaseURL = "postgres://localhost/ipam?sslmode=disable" }), true},
		{"stage api: missing database URL", "api", withField(validStageAPI, func(s *Settings) { s.DatabaseURL = "" }), true},
		{"stage api: missing NetBox token", "api", withField(validStageAPI, func(s *Settings) { s.NetBoxToken = "" }), true},
		{"stage api: NetBox URL not HTTPS", "api", withField(validStageAPI, func(s *Settings) { s.NetBoxURL = "http://netbox.stage.example.org" }), true},
		{"stage api: IPAM_LOCAL_EXTRA_CREDENTIALS outside development", "api", withField(validStageAPI, func(s *Settings) { s.LocalExtraCredentials = "ops-observer:ops-development-token-1234567890" }), true},
		{"stage api: invalid IPAM_UI_INVENTORY_LINKS_ENABLED", "api", withField(validStageAPI, func(s *Settings) { s.UIInventoryLinks = "not-a-bool" }), true},
		{"stage api: invalid IPAM_UI_NETBOX_BASE_URL", "api", withField(validStageAPI, func(s *Settings) { s.UINetBoxURL = "not a url" }), true},
		{"stage api: invalid environment", "api", withField(validStageAPI, func(s *Settings) { s.Environment = "staging" }), true},
		{"stage api: invalid auth mode enum", "api", withField(validStageAPI, func(s *Settings) { s.AuthMode = "saml" }), true},
		{"stage api: invalid AWS mode enum", "api", withField(validStageAPI, func(s *Settings) { s.AWSMode = "mock" }), true},

		{"dev api: LocalToken too short", "api", withField(validDevAPI, func(s *Settings) { s.LocalToken = "short" }), true},
		{"dev api: fake AWS mode missing fake cloud file", "api", withField(validDevAPI, func(s *Settings) { s.FakeCloudFile = "" }), true},
		{"dev api: IPAM_LOCAL_EXTRA_CREDENTIALS permitted in development", "api", withField(validDevAPI, func(s *Settings) { s.LocalExtraCredentials = "ops-observer:ops-development-token-1234567890" }), false},

		{"stage migrate: valid TLS only, no other setting required", "migrate", Settings{Environment: "stage", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full"}, false},
		{"prod migrate: unverified TLS refused", "migrate", Settings{Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=disable"}, true},
		{"prod migrate: missing database URL", "migrate", Settings{Environment: "prod"}, true},
		{"dev migrate: no TLS requirement, nothing else validated", "migrate", Settings{Environment: "development", DatabaseURL: "postgres://localhost/ipam"}, false},
		{"migrate: invalid environment still refused", "migrate", Settings{Environment: "bogus", DatabaseURL: "postgres://localhost/ipam"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate(tc.mode)
			if tc.wantErr && err == nil {
				t.Fatalf("mode %s: expected refusal, got nil", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %s: expected acceptance, got %v", tc.mode, err)
			}
		})
	}
}
