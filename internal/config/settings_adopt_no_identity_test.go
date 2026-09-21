package config

import "testing"

// TestAdoptAbandonNeedsNoIdentityFile is work-plan package H9's proof, at the
// config-loading layer, that `platform-ipam adopt abandon` needs no identity
// file at all -- not because internal/adoptcmd happens not to read it (that
// is internal/adoptcmd/abandon_test.go's job, and every existing test there
// already calls Main with domain.Config{}, i.e. zero identities), but because
// nothing between cmd/platform-ipam/main.go's dispatch and Service.
// AbandonAdoption's call ever demands one exist:
//
//   - Settings.Validate("adopt") never inspects s.IdentityFile at all (see
//     config.go: no branch anywhere reads it), so an environment with
//     IPAM_IDENTITY_FILE unset validates for "adopt" exactly as one with it
//     set.
//   - Load(path, identityFile, environment) reads the identity file only
//     `if identityFile != ""` -- with it empty, cfg.Identities is left at
//     whatever decode(path, &cfg) found in the pools file alone (nothing,
//     for examples/config/pools.yaml, which carries no "identities" key),
//     and Load still succeeds.
//   - internal/adoptcmd's runAbandon (abandon.go) is called from Main
//     without cfg at all ("case \"abandon\": return runAbandon(ctx, rest,
//     svc, stdout, stderr)"), unlike runPlan/runApply, which take cfg to
//     resolve an acting principal via resolvePrincipal(cfg.Identities, r) --
//     confirmed by every case in abandon_test.go already passing with
//     domain.Config{}.
//
// This is the code-level half of H9(a)'s proof; the chart-level half is
// ci/operator-job-adopt-abandon-no-identity-values.yaml and the
// helm-operator-job-adopt-abandon-no-identity check in scripts/ai/checks.py,
// which render templates/operator-job.yaml with mode: adopt, command:
// abandon and NO identity.existingConfigMap set at all.
func TestAdoptAbandonNeedsNoIdentityFile(t *testing.T) {
	t.Run("Settings.Validate(adopt) accepts an empty IdentityFile", func(t *testing.T) {
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
			IdentityFile: "", // no identity file at all
		}
		if err := s.Validate("adopt"); err != nil {
			t.Fatalf("adopt: an empty IdentityFile was refused: %v", err)
		}
	})
	t.Run("Settings.Validate(adopt) accepts an identity file that does not exist on disk", func(t *testing.T) {
		// Validate is a pure function of Settings -- it never opens
		// IdentityFile itself (only Load does, and only when the path is
		// non-empty) -- so a path naming a file that was never created must
		// validate identically to an empty one for adopt, confirming
		// Validate makes no promise about the file's existence either.
		s := Settings{
			Environment: "development", DatabaseURL: "postgres://localhost/ipam",
			AWSMode: "fake", FakeCloudFile: "fixtures/cloud.json",
			NetBoxURL: "http://netbox:8080", NetBoxToken: "netbox-token",
			IdentityFile: "/nonexistent/identities.yaml",
		}
		if err := s.Validate("adopt"); err != nil {
			t.Fatalf("adopt: a nonexistent IdentityFile path was refused by Validate, which never opens it: %v", err)
		}
	})
	t.Run("Load(pools.yaml, \"\", development) succeeds with zero identities", func(t *testing.T) {
		cfg, err := Load("../../examples/config/pools.yaml", "", "development")
		if err != nil {
			t.Fatalf("Load with no identity file: %v", err)
		}
		if len(cfg.Identities) != 0 {
			t.Fatalf("Load with no identity file: expected zero identities, got %d", len(cfg.Identities))
		}
	})
}
