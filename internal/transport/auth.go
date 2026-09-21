package transport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/domain"
)

type Authenticator interface {
	Authenticate(*http.Request) (domain.Principal, error)
}
type Auth struct {
	mode, token, subject string
	identities           map[string]domain.Principal
	verifier             *oidc.IDTokenVerifier
	// credentialSubjects/credentialHashes are the full local-mode credential
	// set, index 0 always the primary IPAM_LOCAL_TOKEN/IPAM_LOCAL_SUBJECT and
	// 1..N the IPAM_LOCAL_EXTRA_CREDENTIALS entries (package G3c), built once
	// in NewAuth so Authenticate never has to branch on which one it is
	// checking.
	credentialSubjects []string
	credentialHashes   [][sha256.Size]byte
}

// extraCredential is one parsed IPAM_LOCAL_EXTRA_CREDENTIALS entry.
type extraCredential struct {
	subject, token string
}

// parseExtraCredentials parses IPAM_LOCAL_EXTRA_CREDENTIALS ("subject:token"
// pairs separated by commas) and validates every rule package G3c requires.
// It is pure and independent of NewAuth's environment/mode gate so each rule
// can be tested on its own.
//
// A malformed entry fails start-up outright rather than being skipped or
// silently repaired: an operator who fat-fingers this variable should see
// the process refuse to start, not run with fewer identities than intended.
// In particular, leading/trailing whitespace around a subject or token is
// rejected rather than trimmed -- trimming would let a copy/paste mistake
// (e.g. a stray trailing space or newline) become a token that silently
// never matches anything, which is a worse failure mode than refusing to
// start. A token may itself contain ':', so each pair is split on only its
// FIRST colon.
func parseExtraCredentials(raw, mainToken, mainSubject string, identities map[string]domain.Principal) ([]extraCredential, error) {
	if raw == "" {
		return nil, nil
	}
	seenSubjects := map[string]bool{mainSubject: true}
	seenTokens := map[string]bool{mainToken: true}
	var out []extraCredential
	// Errors name an entry by its position, never by its text: a malformed
	// entry may be nothing but a token, and start-up errors are logged.
	for i, pair := range strings.Split(raw, ",") {
		colon := strings.IndexByte(pair, ':')
		if colon < 0 {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS entry %d must be subject:token", i+1)
		}
		subject, token := pair[:colon], pair[colon+1:]
		if subject == "" || token == "" {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS entry %d has an empty subject or token", i+1)
		}
		if strings.TrimSpace(subject) != subject || strings.TrimSpace(token) != token {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS entry for %q has leading or trailing whitespace", subject)
		}
		if len(token) < 24 {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS token for %q is shorter than 24 characters", subject)
		}
		if seenTokens[token] {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS token for %q duplicates another configured token", subject)
		}
		if seenSubjects[subject] {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS subject %q duplicates another configured subject", subject)
		}
		if _, ok := identities[subject]; !ok {
			return nil, fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS subject %q has no identity mapping", subject)
		}
		seenSubjects[subject], seenTokens[token] = true, true
		out = append(out, extraCredential{subject: subject, token: token})
	}
	return out, nil
}

func NewAuth(ctx context.Context, s config.Settings, entries []domain.Principal) (*Auth, error) {
	a := &Auth{mode: s.AuthMode, token: s.LocalToken, subject: s.LocalSubject, identities: map[string]domain.Principal{}}
	for _, p := range entries {
		a.identities[p.Subject] = p
	}
	if len(a.identities) == 0 {
		return nil, fmt.Errorf("at least one server-side identity mapping is required")
	}
	if a.mode == "local" {
		// This also refuses IPAM_LOCAL_EXTRA_CREDENTIALS outside development:
		// local mode as a whole is development-only, so nothing past this
		// point -- including the extra-credential parsing below -- can ever
		// run with Environment != "development". config.Settings.Validate
		// carries the matching independent guard, checked before NewAuth is
		// ever called; this is the second, package G3c's "two guards on
		// purpose".
		if s.Environment != "development" || len(a.token) < 24 {
			return nil, fmt.Errorf("local authentication only permits explicit development credentials")
		}
		if _, ok := a.identities[a.subject]; !ok {
			return nil, fmt.Errorf("local subject has no identity mapping")
		}
		extra, err := parseExtraCredentials(s.LocalExtraCredentials, a.token, a.subject, a.identities)
		if err != nil {
			return nil, err
		}
		a.credentialSubjects = append(a.credentialSubjects, a.subject)
		a.credentialHashes = append(a.credentialHashes, sha256.Sum256([]byte(a.token)))
		for _, c := range extra {
			a.credentialSubjects = append(a.credentialSubjects, c.subject)
			a.credentialHashes = append(a.credentialHashes, sha256.Sum256([]byte(c.token)))
		}
		return a, nil
	}
	if a.mode != "oidc" {
		return nil, fmt.Errorf("unsupported authentication mode")
	}
	if s.LocalExtraCredentials != "" {
		// oidc mode ignores IPAM_LOCAL_EXTRA_CREDENTIALS entirely (package
		// G3c); say so once at start-up, without the value, so a leftover
		// development setting is visible rather than silently inert.
		slog.Warn("IPAM_LOCAL_EXTRA_CREDENTIALS is set but ignored in oidc authentication mode")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, s.OIDCIssuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: s.OIDCAudience, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}})
	return a, nil
}
func (a *Auth) Authenticate(r *http.Request) (domain.Principal, error) {
	var empty domain.Principal
	if len(r.Header.Values("Authorization")) != 1 {
		return empty, domain.Err(401, "unauthenticated", "Exactly one bearer credential is required.")
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 16384 {
		return empty, domain.Err(401, "unauthenticated", "A valid platform bearer credential is required.")
	}
	subject := a.subject
	if a.mode == "local" {
		// Compare the presented token's SHA-256 against EVERY configured
		// credential's hash (the primary IPAM_LOCAL_TOKEN plus every
		// IPAM_LOCAL_EXTRA_CREDENTIALS entry, package G3c) in constant time
		// and WITHOUT an early exit: the loop always runs to completion, and
		// which index (if any) matched is selected with
		// subtle.ConstantTimeSelect rather than an if/else keyed on the
		// match bit, so no comparison and no part of the selection branches
		// on secret data.
		//
		// What this hides: given two requests, one presenting a token that
		// matches the Nth configured credential and one presenting a token
		// that matches none, or matches a different credential, the two
		// calls perform the identical sequence of operations (the same
		// number of SHA-256 comparisons, the same constant-time selects) --
		// so elapsed time cannot be used to learn which credential (if any)
		// matched, or its position among the configured set.
		//
		// What this does NOT hide: (1) whether authentication ultimately
		// succeeds or fails at all -- the caller always learns that from the
		// response, and there is no way to authenticate a request without
		// that being observable; (2) how many credentials are configured --
		// that is fixed server configuration, identical for every request
		// regardless of its content, not something request-scoped that a
		// caller could use to extract information; (3) microarchitectural
		// side channels (cache timing, branch prediction) on the final
		// slice index lookup below, once matchedIndex is known -- only the
		// credential-comparison loop itself is held to this constant-time
		// discipline.
		presented := sha256.Sum256([]byte(parts[1]))
		matchedIndex := -1
		for i, hash := range a.credentialHashes {
			match := subtle.ConstantTimeCompare(presented[:], hash[:])
			matchedIndex = subtle.ConstantTimeSelect(match, i, matchedIndex)
		}
		if matchedIndex < 0 {
			return empty, domain.Err(401, "unauthenticated", "Invalid platform credential.")
		}
		// The subject is never taken from the caller: it is always the
		// subject associated with whichever configured credential matched.
		subject = a.credentialSubjects[matchedIndex]
	} else {
		token, err := a.verifier.Verify(r.Context(), parts[1])
		if err != nil {
			return empty, domain.Err(401, "unauthenticated", "Invalid or expired platform credential.")
		}
		var claims struct {
			TokenUse string `json:"token_use"`
		}
		if err := token.Claims(&claims); err != nil || (claims.TokenUse != "" && claims.TokenUse != "access") {
			return empty, domain.Err(401, "unauthenticated", "An API access credential is required.")
		}
		subject = token.Subject
	}
	p, ok := a.identities[subject]
	if !ok {
		return empty, domain.Err(403, "forbidden", "Identity is not onboarded to platform-ipam.")
	}
	return p, nil
}
