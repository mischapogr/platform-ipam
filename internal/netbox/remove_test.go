package netbox

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Package M9b4 (ADR 0016): onboard remove. These tests reuse adoptStack
// (adopt_test.go) exactly as refresh_test.go and cancel_test.go do: it
// already answers a conditional detail read with an ETag, a DELETE with a
// configurable status, and a confirmation read, which is precisely the
// surface ReadOccupancy and DeleteOccupancy need.

// removalPrefix is importedPrefix plus a description and a contributor
// list, the shape a real M9b1-created prefix carries.
func removalPrefix(id int, cidr string, description string, contributors []domain.Contributor) map[string]any {
	x := importedPrefix(id, cidr, 7)
	x["description"] = description
	if contributors != nil {
		// withContributors marshals contributors as given, so a non-nil but
		// zero-length slice still stores a real (empty) JSON array -- the
		// "list nobody wrote" case ADR 0016 keeps distinct from "no list at
		// all" -- while a nil slice (the default, below) leaves the field
		// unset entirely.
		return withContributors(x, contributors...)
	}
	return x
}

func removalContributor(resource, cidr, observedAt string) domain.Contributor {
	return refreshContributor(resource, cidr, observedAt)
}

func TestReadOccupancyReturnsTheCurrentState(t *testing.T) {
	contributors := []domain.Contributor{removalContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: removalPrefix(42, "10.0.1.0/24", "accounts 000000000001 | resource ids vpc-a", contributors)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ID != "42" || detail.CIDR != "10.0.1.0/24" {
		t.Fatalf("unexpected id/cidr: %#v", detail)
	}
	if detail.Description != "accounts 000000000001 | resource ids vpc-a" {
		t.Fatalf("unexpected description %q", detail.Description)
	}
	if detail.Owned || !detail.Imported || detail.Unreadable || detail.Reconstructed {
		t.Fatalf("unexpected flags: %#v", detail)
	}
	if detail.PoolConflict != "" {
		t.Fatalf("unexpected pool conflict %q", detail.PoolConflict)
	}
	if len(detail.Contributors) != 1 || detail.Contributors[0].Identity != contributors[0].Identity {
		t.Fatalf("unexpected contributors %#v", detail.Contributors)
	}
	// Read-only: nothing was written.
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("ReadOccupancy wrote %v", writes)
	}
}

func TestReadOccupancyReportsNoContributorListAtAll(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: removalPrefix(42, "10.0.1.0/24", "", nil)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Contributors != nil {
		t.Fatalf("expected a nil contributor list, got %#v", detail.Contributors)
	}
	if detail.Unreadable {
		t.Fatalf("a missing list must not read as unreadable")
	}
}

func TestReadOccupancyReportsAnEmptyContributorList(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: removalPrefix(42, "10.0.1.0/24", "", []domain.Contributor{})}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Contributors == nil || len(detail.Contributors) != 0 {
		t.Fatalf("expected a non-nil, zero-length contributor list, got %#v", detail.Contributors)
	}
}

func TestReadOccupancyReportsAnUnreadableList(t *testing.T) {
	x := removalPrefix(42, "10.0.1.0/24", "", nil)
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsField] = "not a list"
	stack := &adoptStack{prefixes: map[int]map[string]any{42: x}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Unreadable {
		t.Fatalf("expected the list to read as unreadable")
	}
}

func TestReadOccupancyReportsTheReconstructedFlag(t *testing.T) {
	contributors := []domain.Contributor{removalContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	x := removalPrefix(42, "10.0.1.0/24", "", contributors)
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsReconstructedField] = true
	stack := &adoptStack{prefixes: map[int]map[string]any{42: x}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Reconstructed {
		t.Fatalf("expected Reconstructed to be true")
	}
}

func TestReadOccupancyReportsAPoolConflict(t *testing.T) {
	// The pool's own CIDR is 10.0.0.0/16 (testDomain); asking about it
	// directly is the equality case refusePoolPrefix names.
	stack := &adoptStack{prefixes: map[int]map[string]any{1: removalPrefix(1, "10.0.0.0/16", "", nil)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	detail, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if detail.PoolConflict == "" {
		t.Fatalf("expected a pool conflict to be reported")
	}
}

func TestReadOccupancyRefusesWhenNoPrefixHoldsTheCIDR(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{}}
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.ReadOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24")
	if !errors.Is(err, ErrRemovalNotFound) {
		t.Fatalf("expected ErrRemovalNotFound, got %v", err)
	}
}

// --- DeleteOccupancy ---

func deletableStack() (*adoptStack, []domain.Contributor) {
	contributors := []domain.Contributor{removalContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: removalPrefix(42, "10.0.1.0/24", "", contributors)}}
	return stack, contributors
}

func TestDeleteOccupancyDeletesAndConfirms404(t *testing.T) {
	stack, _ := deletableStack()
	c, closer := adoptClient(t, stack)
	defer closer()

	result, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "42" || result.CIDR != "10.0.1.0/24" {
		t.Fatalf("unexpected result %#v", result)
	}
	wantRequests := []string{
		"GET /api/ipam/prefixes/", "GET /api/ipam/prefixes/42/",
		"DELETE /api/ipam/prefixes/42/", "GET /api/ipam/prefixes/42/",
	}
	if len(stack.requests) != len(wantRequests) {
		t.Fatalf("requests %v, want %v", stack.requests, wantRequests)
	}
	for i, want := range wantRequests {
		if stack.requests[i] != want {
			t.Fatalf("request %d is %q, want %q", i, stack.requests[i], want)
		}
	}
	if stack.ifMatch[0] != stack.etag {
		t.Fatalf("the delete was not conditional: If-Match %q, ETag %q", stack.ifMatch[0], stack.etag)
	}
	if _, present := stack.prefixes[42]; present {
		t.Fatalf("the prefix is still there")
	}
}

func TestDeleteOccupancyRefusesAnOwnedPrefix(t *testing.T) {
	stack, contributors := deletableStack()
	x := stack.prefixes[42]
	fields, _ := x["custom_fields"].(map[string]any)
	fields[allocationIDCF] = "alloc-1"
	stack.prefixes[42] = withContributors(x, contributors...)
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("expected ErrOccupancyManaged, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAPrefixWithoutTheImportTag(t *testing.T) {
	stack, contributors := deletableStack()
	x := stack.prefixes[42]
	x["tags"] = []any{}
	stack.prefixes[42] = withContributors(x, contributors...)
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("expected ErrOccupancyManaged, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAPoolPrefix(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{1: removalPrefix(1, "10.0.0.0/16", "",
		[]domain.Contributor{removalContributor("vpc-a", "10.0.0.0/16", "2026-09-22T10:00:00Z")})}}
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.0.0/16", "1")
	if !errors.Is(err, ErrOccupancyPool) {
		t.Fatalf("expected ErrOccupancyPool, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAnUnreadableList(t *testing.T) {
	stack, _ := deletableStack()
	x := stack.prefixes[42]
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsField] = "not a list"
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrRefreshUnreadable) {
		t.Fatalf("expected ErrRefreshUnreadable, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAnEmptyContributorList(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: removalPrefix(42, "10.0.1.0/24", "", []domain.Contributor{})}}
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrOccupancyInvalid) {
		t.Fatalf("expected ErrOccupancyInvalid, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAReconstructedList(t *testing.T) {
	stack, _ := deletableStack()
	x := stack.prefixes[42]
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsReconstructedField] = true
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrRemovalReconstructed) {
		t.Fatalf("expected ErrRemovalReconstructed, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesAnIDMismatch(t *testing.T) {
	stack, _ := deletableStack()
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "99")
	if !errors.Is(err, ErrRemovalIDMismatch) {
		t.Fatalf("expected ErrRemovalIDMismatch, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no writes, got %v", writes)
	}
}

func TestDeleteOccupancyRefusesOnAConditionalConflict(t *testing.T) {
	stack, _ := deletableStack()
	stack.deleteStatus = http.StatusPreconditionFailed
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrOccupancyConflict) {
		t.Fatalf("expected ErrOccupancyConflict, got %v", err)
	}
	if _, present := stack.prefixes[42]; !present {
		t.Fatalf("a 412 must leave the prefix in place")
	}
}

func TestDeleteOccupancyRefusesWhenTheReadBackStillFindsIt(t *testing.T) {
	stack, _ := deletableStack()
	// A foreign write recreates the object between the delete and the
	// confirmation read, exactly as adopt_test.go's own afterDelete tests
	// exercise for CancelReservation.
	stack.afterDelete = func(s *adoptStack) {
		s.prefixes[42] = removalPrefix(42, "10.0.1.0/24", "", []domain.Contributor{removalContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")})
	}
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, ErrRemovalReadBack) {
		t.Fatalf("expected ErrRemovalReadBack, got %v", err)
	}
}

func TestDeleteOccupancyIsUncertainOnATransportFailure(t *testing.T) {
	stack, _ := deletableStack()
	stack.deleteStatus = http.StatusInternalServerError
	c, closer := adoptClient(t, stack)
	defer closer()

	_, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if !errors.Is(err, domain.ErrInventoryUncertain) || !errors.Is(err, ErrRemovalUncertain) {
		t.Fatalf("expected an uncertain removal, got %v", err)
	}
}

func TestDeleteOccupancyTreatsA404OnTheDeleteAsAlreadyGone(t *testing.T) {
	stack, _ := deletableStack()
	// Something else deletes the prefix between the detail read and this
	// call's own DELETE, so the real fake server (not a forced status)
	// answers the DELETE with a genuine 404 -- adoptStack's beforeRead runs
	// once, during the detail GET that supplies the ETag, and the prefix it
	// removes is gone by the time the DELETE request arrives.
	stack.beforeRead = func(map[string]any) { delete(stack.prefixes, 42) }
	c, closer := adoptClient(t, stack)
	defer closer()

	result, err := c.DeleteOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "42")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "42" {
		t.Fatalf("unexpected result %#v", result)
	}
}
