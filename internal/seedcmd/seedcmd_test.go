package seedcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeNetBox serves the three endpoints internal/netbox.Client.Seed calls.
// preseedFields lets a test start with an existing field already present (to
// exercise the "present" and "conflict" outcomes).
func fakeNetBox(t *testing.T, preseedFields map[string]map[string]any) *httptest.Server {
	t.Helper()
	choiceSets := map[string]map[string]any{}
	fields := map[string]map[string]any{}
	for k, v := range preseedFields {
		fields[k] = v
	}
	tags := map[string]map[string]any{}
	nextID := 1000
	mount := func(mux *http.ServeMux, path string, store map[string]map[string]any, key string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				want := r.URL.Query().Get(key)
				var results []map[string]any
				if obj, ok := store[want]; ok {
					results = append(results, obj)
				}
				json.NewEncoder(w).Encode(map[string]any{"count": len(results), "next": nil, "results": results})
			case http.MethodPost:
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				nextID++
				body["id"] = nextID
				name, _ := body[key].(string)
				store[name] = body
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(body)
			default:
				http.NotFound(w, r)
			}
		})
	}
	mux := http.NewServeMux()
	mount(mux, "/api/extras/custom-field-choice-sets/", choiceSets, "name")
	mount(mux, "/api/extras/custom-fields/", fields, "name")
	mount(mux, "/api/extras/tags/", tags, "slug")
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func setSeedEnv(t *testing.T, netboxURL string) {
	t.Helper()
	t.Setenv("IPAM_ENVIRONMENT", "development")
	t.Setenv("IPAM_NETBOX_URL", netboxURL)
	t.Setenv("IPAM_NETBOX_TOKEN", "seed-test-token")
	// Clear settings a stray parent-process environment could otherwise
	// leak in, to keep these tests independent of the shell that runs them.
	for _, k := range []string{"IPAM_DATABASE_URL", "IPAM_AUTH_MODE", "IPAM_LOCAL_TOKEN", "IPAM_OIDC_ISSUER", "IPAM_OIDC_AUDIENCE", "IPAM_AWS_MODE"} {
		t.Setenv(k, "")
	}
}

func TestMainRejectsAnyArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"plan"}, &out, &errOut)
	if code != ExitUsage {
		t.Fatalf("code = %d, want %d", code, ExitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on a usage error, got %q", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("stderr should explain the usage error")
	}
}

func TestMainFailsAdapterWhenSettingsAreMissing(t *testing.T) {
	t.Setenv("IPAM_ENVIRONMENT", "development")
	t.Setenv("IPAM_NETBOX_URL", "")
	t.Setenv("IPAM_NETBOX_TOKEN", "")
	var out, errOut bytes.Buffer
	code := Main(context.Background(), nil, &out, &errOut)
	if code != ExitAdapter {
		t.Fatalf("code = %d, want %d", code, ExitAdapter)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty on an adapter failure, got %q", out.String())
	}
}

func TestMainSucceedsAndReportsEveryObjectCreated(t *testing.T) {
	s := fakeNetBox(t, nil)
	setSeedEnv(t, s.URL)
	var out, errOut bytes.Buffer
	code := Main(context.Background(), nil, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("code = %d, want %d; stderr=%s", code, ExitOK, errOut.String())
	}
	var report Report
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &report); err != nil {
		t.Fatalf("decoding report: %v\nstdout: %s", err, out.String())
	}
	if len(report.Objects) == 0 {
		t.Fatal("report carries no objects")
	}
	for _, obj := range report.Objects {
		if obj.Action != "created" {
			t.Errorf("%s %s: action = %s, want created", obj.Kind, obj.Name, obj.Action)
		}
	}
}

func TestMainIsIdempotentAcrossTwoRuns(t *testing.T) {
	s := fakeNetBox(t, nil)
	setSeedEnv(t, s.URL)
	var first bytes.Buffer
	if code := Main(context.Background(), nil, &first, &bytes.Buffer{}); code != ExitOK {
		t.Fatalf("first run: code = %d", code)
	}
	var second, errOut bytes.Buffer
	code := Main(context.Background(), nil, &second, &errOut)
	if code != ExitOK {
		t.Fatalf("second run: code = %d, stderr=%s", code, errOut.String())
	}
	var report Report
	if err := json.Unmarshal(bytes.TrimSpace(second.Bytes()), &report); err != nil {
		t.Fatalf("decoding second report: %v", err)
	}
	for _, obj := range report.Objects {
		if obj.Action != "present" {
			t.Errorf("second run: %s %s: action = %s, want present (nothing to do)", obj.Kind, obj.Name, obj.Action)
		}
	}
}

func TestMainExitsConflictWhenAnExistingFieldDisagrees(t *testing.T) {
	s := fakeNetBox(t, map[string]map[string]any{
		"platform_allocation_id": {"id": 1, "name": "platform_allocation_id", "type": "integer", "object_types": []any{"ipam.prefix"}},
	})
	setSeedEnv(t, s.URL)
	var out, errOut bytes.Buffer
	code := Main(context.Background(), nil, &out, &errOut)
	if code != ExitConflict {
		t.Fatalf("code = %d, want %d; stderr=%s", code, ExitConflict, errOut.String())
	}
	var report Report
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &report); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	found := false
	for _, obj := range report.Objects {
		if obj.Name == "platform_allocation_id" {
			found = true
			if obj.Action != "conflict" {
				t.Errorf("platform_allocation_id: action = %s, want conflict", obj.Action)
			}
			if obj.Detail == "" {
				t.Error("conflict result carries no detail")
			}
		}
	}
	if !found {
		t.Fatal("report does not mention the conflicting field")
	}
	// The conflict must not stop the rest of the catalogue from being
	// reported and created.
	other := false
	for _, obj := range report.Objects {
		if obj.Name == "platform_allocation_key" && obj.Action == "created" {
			other = true
		}
	}
	if !other {
		t.Error("a conflict on one field should not stop the rest of the catalogue from being created")
	}
}

func TestMainExitsAdapterWhenNetBoxIsUnreachable(t *testing.T) {
	setSeedEnv(t, "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	code := Main(context.Background(), nil, &out, &errOut)
	if code != ExitAdapter {
		t.Fatalf("code = %d, want %d", code, ExitAdapter)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty when NetBox is unreachable, got %q", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("stderr should explain the failure")
	}
}
