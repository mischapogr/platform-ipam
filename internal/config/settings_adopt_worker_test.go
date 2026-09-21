package config

import "testing"

// TestSettingsValidateAdoptAndWorkerModeAware is package H5's evidence that
// the mode-aware rewrite gives "adopt" (and, per the evidence recorded in the
// H5 report, "worker") exactly the settings each mode uses, in stage and
// prod: no OIDC issuer, no audience, no auth mode, no listen address and no
// local token, while still requiring everything adopt/worker's actual code
// path touches (the database, NetBox, and -- unlike development -- live AWS
// observations).
func TestSettingsValidateAdoptAndWorkerModeAware(t *testing.T) {
	for _, environment := range []string{"stage", "prod"} {
		t.Run(environment, func(t *testing.T) {
			base := Settings{
				Environment: environment,
				DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
				// No OIDC issuer/audience, no IPAM_LISTEN_ADDR, no
				// IPAM_LOCAL_TOKEN: neither mode's server ever authenticates.
				AWSMode:     "live",
				NetBoxURL:   "https://netbox." + environment + ".example.org",
				NetBoxToken: "netbox-token",
			}
			for _, mode := range []string{"adopt", "worker"} {
				// adopt reads IPAM_AUTH_MODE nowhere at all (the enum check
				// above is skipped for it), so its minimal settings leave it
				// unset; worker still serves /livez and /readyz and keeps
				// that enum check, so its minimal settings need a valid
				// value -- "local" is the cheaper of the two to construct.
				minimal := base
				if mode == "worker" {
					minimal.AuthMode = "local"
				}
				t.Run(mode+"/minimal settings validate", func(t *testing.T) {
					if err := minimal.Validate(mode); err != nil {
						t.Fatalf("%s in %s: expected acceptance with no OIDC/listen/local-token settings, got %v", mode, environment, err)
					}
				})
				t.Run(mode+"/IPAM_AUTH_MODE=local also validates", func(t *testing.T) {
					s := minimal
					s.AuthMode = "local"
					if err := s.Validate(mode); err != nil {
						t.Fatalf("%s in %s: expected acceptance with AuthMode=local, got %v", mode, environment, err)
					}
				})
				t.Run(mode+"/missing database URL still refused", func(t *testing.T) {
					s := minimal
					s.DatabaseURL = ""
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: missing database URL accepted", mode, environment)
					}
				})
				t.Run(mode+"/non-verify-full sslmode still refused", func(t *testing.T) {
					s := minimal
					s.DatabaseURL = "postgres://localhost/ipam?sslmode=require"
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: non-verify-full sslmode accepted", mode, environment)
					}
				})
				t.Run(mode+"/missing NetBox token still refused", func(t *testing.T) {
					s := minimal
					s.NetBoxToken = ""
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: missing NetBox token accepted", mode, environment)
					}
				})
				t.Run(mode+"/missing NetBox URL still refused", func(t *testing.T) {
					s := minimal
					s.NetBoxURL = ""
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: missing NetBox URL accepted", mode, environment)
					}
				})
				t.Run(mode+"/fake AWS mode still refused (live only in stage/prod)", func(t *testing.T) {
					s := minimal
					s.AWSMode = "fake"
					s.FakeCloudFile = "fixtures/cloud.json"
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: IPAM_AWS_MODE=fake accepted", mode, environment)
					}
				})
				t.Run(mode+"/IPAM_LOCAL_EXTRA_CREDENTIALS still refused outside development", func(t *testing.T) {
					s := minimal
					s.LocalExtraCredentials = "ops-observer:ops-development-token-1234567890"
					if s.Validate(mode) == nil {
						t.Fatalf("%s in %s: IPAM_LOCAL_EXTRA_CREDENTIALS accepted outside development", mode, environment)
					}
				})
			}
		})
	}

	// worker keeps the general IPAM_AUTH_MODE enum check and the UI settings
	// (it serves /livez and /readyz, and a garbage IPAM_AUTH_MODE or an
	// invalid IPAM_UI_* value is not a defect package H5 was asked to
	// investigate for it), unlike adopt which reads neither.
	t.Run("worker still enforces the auth-mode enum", func(t *testing.T) {
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AuthMode: "saml", AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
		}
		if s.Validate("worker") == nil {
			t.Fatal("worker: invalid IPAM_AUTH_MODE accepted")
		}
	})
	t.Run("adopt does not enforce the auth-mode enum", func(t *testing.T) {
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AuthMode: "saml", AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
		}
		if err := s.Validate("adopt"); err != nil {
			t.Fatalf("adopt: a garbage IPAM_AUTH_MODE, which adopt never reads, was refused: %v", err)
		}
	})
	t.Run("adopt does not enforce the UI settings", func(t *testing.T) {
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
			UIInventoryLinks: "not-a-bool", UINetBoxURL: "not a url",
		}
		if err := s.Validate("adopt"); err != nil {
			t.Fatalf("adopt: invalid UI settings, which adopt never reads, were refused: %v", err)
		}
	})
	t.Run("worker still enforces the UI settings", func(t *testing.T) {
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AuthMode: "local", LocalToken: "local-development-token-123456",
			AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
			UIInventoryLinks: "not-a-bool",
		}
		if s.Validate("worker") == nil {
			t.Fatal("worker: invalid IPAM_UI_INVENTORY_LINKS_ENABLED accepted")
		}
	})

	// adopt still refuses an oidc issuer/audience that IS present but
	// malformed only if it would ever be read; since adopt never reads them,
	// a malformed pair must not block it either.
	t.Run("adopt ignores a malformed OIDC issuer inherited from another process's environment", func(t *testing.T) {
		s := Settings{
			Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
			AuthMode: "oidc", OIDCIssuer: "not-a-url", OIDCAudience: "",
			AWSMode: "live", NetBoxURL: "https://netbox.prod.example.org", NetBoxToken: "netbox-token",
		}
		if err := s.Validate("adopt"); err != nil {
			t.Fatalf("adopt: inherited, malformed OIDC settings were refused: %v", err)
		}
	})
	t.Run("worker also ignores a malformed OIDC issuer, sharing adopt's fix", func(t *testing.T) {
		s := Settings{
			Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
			AuthMode: "oidc", OIDCIssuer: "not-a-url", OIDCAudience: "",
			AWSMode: "live", NetBoxURL: "https://netbox.prod.example.org", NetBoxToken: "netbox-token",
		}
		if err := s.Validate("worker"); err != nil {
			t.Fatalf("worker: inherited, malformed OIDC settings were refused: %v", err)
		}
	})

	// api is untouched: it still requires OIDC issuer/audience in stage/prod
	// and validates them when AuthMode == "oidc" regardless of mode
	// inheritance concerns, because api is the one mode that actually reads
	// them.
	t.Run("api still refuses a malformed OIDC issuer", func(t *testing.T) {
		s := Settings{
			Environment: "prod", DatabaseURL: "postgres://localhost/ipam?sslmode=verify-full",
			AuthMode: "oidc", OIDCIssuer: "not-a-url", OIDCAudience: "",
			AWSMode: "live", NetBoxURL: "https://netbox.prod.example.org", NetBoxToken: "netbox-token",
		}
		if s.Validate("api") == nil {
			t.Fatal("api: malformed OIDC issuer/missing audience accepted")
		}
	})
}
