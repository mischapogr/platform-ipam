package config

import "testing"

// TestSettingsValidateSeedModeAware is package N4's evidence that "seed"
// (docs/WORK_PLAN.md, found by M9b1) requires only the NetBox origin and
// token, in every environment: no database URL, no auth/OIDC/local-token
// settings, no AWS mode, no UI settings and no fake-cloud fixture -- unlike
// every mode this package touched before it (H5's adopt and worker still
// open the database and, in stage/prod, demand live AWS observations; seed
// opens no database at all).
func TestSettingsValidateSeedModeAware(t *testing.T) {
	for _, environment := range []string{"development", "stage", "prod"} {
		t.Run(environment, func(t *testing.T) {
			netboxURL := "https://netbox." + environment + ".example.org"
			if environment == "development" {
				netboxURL = "http://netbox:8080"
			}
			minimal := Settings{
				Environment: environment,
				NetBoxURL:   netboxURL,
				NetBoxToken: "netbox-token",
			}
			t.Run("minimal settings validate", func(t *testing.T) {
				if err := minimal.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: expected acceptance with only NetBox settings, got %v", environment, err)
				}
			})
			t.Run("no database URL required", func(t *testing.T) {
				s := minimal
				s.DatabaseURL = ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused with no database URL: %v", environment, err)
				}
			})
			t.Run("no OIDC issuer/audience required", func(t *testing.T) {
				s := minimal
				s.OIDCIssuer, s.OIDCAudience = "", ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused with no OIDC settings: %v", environment, err)
				}
			})
			t.Run("malformed OIDC issuer inherited from another process's environment is ignored", func(t *testing.T) {
				s := minimal
				s.AuthMode, s.OIDCIssuer, s.OIDCAudience = "oidc", "not-a-url", ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused by inherited, malformed OIDC settings it never reads: %v", environment, err)
				}
			})
			t.Run("no auth mode required, and garbage is ignored", func(t *testing.T) {
				s := minimal
				s.AuthMode = "saml"
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused by an IPAM_AUTH_MODE it never reads: %v", environment, err)
				}
			})
			t.Run("no local token required", func(t *testing.T) {
				s := minimal
				s.AuthMode, s.LocalToken = "local", ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused with no IPAM_LOCAL_TOKEN: %v", environment, err)
				}
			})
			t.Run("no AWS mode required (live not demanded even in stage/prod)", func(t *testing.T) {
				s := minimal
				s.AWSMode = ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused with no IPAM_AWS_MODE: %v", environment, err)
				}
			})
			t.Run("fake AWS mode with no fixture is not refused", func(t *testing.T) {
				s := minimal
				s.AWSMode, s.FakeCloudFile = "fake", ""
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused by IPAM_AWS_MODE=fake with no fixture, which it never reads: %v", environment, err)
				}
			})
			t.Run("no UI settings required, and garbage is ignored", func(t *testing.T) {
				s := minimal
				s.UIInventoryLinks, s.UINetBoxURL = "not-a-bool", "not a url"
				if err := s.Validate("seed"); err != nil {
					t.Fatalf("seed in %s: refused by invalid UI settings it never reads: %v", environment, err)
				}
			})
			t.Run("missing NetBox token still refused", func(t *testing.T) {
				s := minimal
				s.NetBoxToken = ""
				if s.Validate("seed") == nil {
					t.Fatalf("seed in %s: missing NetBox token accepted", environment)
				}
			})
			t.Run("missing NetBox URL still refused", func(t *testing.T) {
				s := minimal
				s.NetBoxURL = ""
				if s.Validate("seed") == nil {
					t.Fatalf("seed in %s: missing NetBox URL accepted", environment)
				}
			})
			t.Run("credential-bearing NetBox URL still refused", func(t *testing.T) {
				s := minimal
				if environment == "development" {
					s.NetBoxURL = "http://user:pass@netbox:8080"
				} else {
					s.NetBoxURL = "https://user:pass@netbox." + environment + ".example.org"
				}
				if s.Validate("seed") == nil {
					t.Fatalf("seed in %s: NetBox URL with embedded credentials accepted", environment)
				}
			})
			if environment != "development" {
				t.Run("plain HTTP NetBox URL still refused outside development", func(t *testing.T) {
					s := minimal
					s.NetBoxURL = "http://netbox." + environment + ".example.org"
					if s.Validate("seed") == nil {
						t.Fatalf("seed in %s: plain HTTP NetBox URL accepted", environment)
					}
				})
			}
			t.Run("IPAM_LOCAL_EXTRA_CREDENTIALS is still refused outside development", func(t *testing.T) {
				s := minimal
				s.LocalExtraCredentials = "ops-observer:ops-development-token-1234567890"
				err := s.Validate("seed")
				if environment == "development" {
					if err != nil {
						t.Fatalf("seed in development: refused a development-only extra credential: %v", err)
					}
				} else if err == nil {
					t.Fatalf("seed in %s: IPAM_LOCAL_EXTRA_CREDENTIALS accepted outside development", environment)
				}
			})
		})
	}
	t.Run("an invalid IPAM_ENVIRONMENT is refused before the seed branch is even reached", func(t *testing.T) {
		s := Settings{Environment: "qa", NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token"}
		if s.Validate("seed") == nil {
			t.Fatal("seed: invalid IPAM_ENVIRONMENT accepted")
		}
	})
	t.Run("other modes are unaffected: api still requires a database URL", func(t *testing.T) {
		s := Settings{
			Environment: "development", NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
			AuthMode: "local", LocalToken: "local-development-token-123456",
			AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
		}
		if s.Validate("api") == nil {
			t.Fatal("api: missing database URL accepted after adding the seed branch")
		}
	})
	t.Run("other modes are unaffected: migrate still requires a database URL", func(t *testing.T) {
		s := Settings{Environment: "development", NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token"}
		if s.Validate("migrate") == nil {
			t.Fatal("migrate: missing database URL accepted after adding the seed branch")
		}
	})
}
