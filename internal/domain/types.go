package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

const (
	Reserved    = "RESERVED"
	Active      = "ACTIVE"
	Quarantined = "QUARANTINED"
	Released    = "RELEASED"
)

type Request struct {
	AllocationKey      string            `json:"allocation_key"`
	Scope              string            `json:"scope"`
	Environment        string            `json:"environment"`
	Region             string            `json:"region"`
	AccountID          string            `json:"account_id,omitempty"`
	AddressFamily      string            `json:"address_family,omitempty"`
	PrefixLength       int               `json:"prefix_length"`
	ParentAllocationID string            `json:"parent_allocation_id,omitempty"`
	AvailabilityZoneID string            `json:"availability_zone_id,omitempty"`
	Description        string            `json:"description"`
	Labels             map[string]string `json:"labels"`
}

// OperatorRole is Principal.Role's only legal non-empty value (ADR 0011 stage
// two): an operator is a principal with no tenant. IsOperator is the one
// predicate this project defines for it; internal/config.Validate is the only
// other package permitted to read Role, and it does so through this method
// rather than by comparing against OperatorRole or the string "operator"
// itself -- see internal/config's source-parsing test for what that buys.
const OperatorRole = "operator"

type Principal struct {
	Subject      string   `json:"subject" yaml:"subject"`
	TenantID     string   `json:"tenant_id" yaml:"tenant_id"`
	Accounts     []string `json:"accounts" yaml:"accounts"`
	Environments []string `json:"environments" yaml:"environments"`
	Regions      []string `json:"regions" yaml:"regions"`
	// Role is empty for an ordinary tenant principal, or OperatorRole for an
	// operator -- a principal with no tenant (ADR 0011). It is decoded only
	// from the server-side identity file (internal/config.Load's YAML decode)
	// and never travels on a bearer token or an OIDC claim.
	Role string `json:"role,omitempty" yaml:"role"`
}

// IsOperator reports whether p is the operator role. Every comparison of
// p.Role against OperatorRole in this project goes through this method; no
// other package compares the role string directly (see internal/config's
// source-parsing test).
func (p Principal) IsOperator() bool { return p.Role == OperatorRole }

type Binding struct {
	Provider     string     `json:"provider"`
	ResourceType string     `json:"resource_type"`
	ResourceID   string     `json:"resource_id"`
	AccountID    string     `json:"account_id"`
	Region       string     `json:"region"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

type Allocation struct {
	Request
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	DomainID           string     `json:"domain_id"`
	RequestHash        string     `json:"request_hash"`
	CIDR               string     `json:"cidr"`
	PoolID             string     `json:"pool_id"`
	State              string     `json:"state"`
	Revision           int64      `json:"revision"`
	PolicyVersion      string     `json:"policy_version"`
	InventoryID        string     `json:"inventory_id"`
	InventorySync      string     `json:"inventory_sync"`
	Binding            *Binding   `json:"binding"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	ReleaseRequestedAt *time.Time `json:"release_requested_at"`
	QuarantineUntil    *time.Time `json:"quarantine_until"`
	LastObservedAt     *time.Time `json:"last_observed_at"`
	ReleaseBlockers    []string   `json:"release_blockers"`
	Committed          bool       `json:"committed"`
}

type APIError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
	Status    int            `json:"-"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// ErrInventoryUncertain marks an inventory adapter error that says nothing
// about the object it was asked about: the adapter could not get an answer (a
// transport failure, a timeout) or got one that is not about the object (a 5xx,
// a 429). The caller does not know whether a write happened. It is the one
// thing an adapter tells the service about its errors, so the service never
// imports an adapter to classify them: an uncertain outcome is retried in
// silence, anything else is a decision the adapter reached on evidence it read
// and a person is told (ADR 0010, ADR 0012).
var ErrInventoryUncertain = errors.New("inventory outcome is uncertain")

func Err(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Retryable: status == 503 || status == 429}
}

type Operation struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	AllocationID string            `json:"allocation_id"`
	TenantID     string            `json:"tenant_id"`
	DomainID     string            `json:"domain_id"`
	Result       map[string]string `json:"result"`
	Error        *APIError         `json:"error"`
	Candidate    *Binding          `json:"candidate,omitempty"`
	Adoption     *AdoptionRecord   `json:"adoption,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// AdoptionRecord is the durable half of the record an operator reviewed before
// an adoption. It rides on the pending operation as a binding candidate rides
// on a verification, and for the same reason: the process that plans one may
// not be the process that finishes it. A crash leaves the ledger's intent and
// nothing else, so the inventory ID whose agreement the commit turns on, the
// cloud resource the observation rule may exempt and the operator the audit
// trail names have to survive it. None of it is a decision, only the evidence
// the code that takes them was given.
type AdoptionRecord struct {
	Operator    string `json:"operator"`
	NetworkID   string `json:"network_id"`
	ResourceID  string `json:"resource_id"`
	ImportBatch string `json:"import_batch,omitempty"`
	// PriorStatus is the inventory status the reviewed network carried before
	// an adoption set it to the one a managed prefix carries. Nothing else
	// remembers it: the ledger never held it and the adoption itself overwrote
	// it, so an adoption that is abandoned would otherwise restore the import's
	// own convention rather than whatever an operator had set (ADR 0012). It is
	// evidence, never a decision -- Snapshot reads no status, so nothing the
	// allocator decides depends on it. A hold created before this field existed
	// carries none, and the clear falls back to the import's status.
	PriorStatus string `json:"prior_status,omitempty"`
	// PriorAccountID and PriorRegion are the AWS account and region the import
	// wrote on the reviewed network. An adoption owns those two inventory
	// fields and overwrites them with the allocation's, so a clear that only
	// emptied them would lose what the import recorded; carried here, the clear
	// puts them back. Evidence, never a decision, exactly as PriorStatus.
	PriorAccountID string `json:"prior_account_id,omitempty"`
	PriorRegion    string `json:"prior_region,omitempty"`
}

// PriorInventory is what an inventory network carried before an adoption
// overwrote it, handed back to the inventory when that adoption is abandoned
// (ADR 0012). Every field is optional: a hold created before the field existed
// has none, and the inventory then falls back to what an import would write.
type PriorInventory struct {
	Status    string
	AccountID string
	Region    string
}

// Prior returns the part of the record an abandon hands back to the inventory.
func (r AdoptionRecord) Prior() PriorInventory {
	return PriorInventory{Status: r.PriorStatus, AccountID: r.PriorAccountID, Region: r.PriorRegion}
}

type Idempotency struct{ TenantID, Method, Path, Key, Hash, AllocationID, OperationID string }
type Event struct {
	ID, AllocationID, TenantID, Actor, Action, Reason string
	At                                                time.Time
	Revision                                          int64
}
type Finding struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenant_id"`
	DomainID     string `json:"domain_id"`
	AllocationID string `json:"allocation_id,omitempty"`
	Code         string `json:"code"`
	Severity     string `json:"severity"`
	Status       string `json:"status"`
	AccountID    string `json:"account_id,omitempty"`
	Region       string `json:"region,omitempty"`
	// ResourceType and ResourceID name the one cloud resource a domain-level
	// finding is about. They exist because the fan-out's key is a hash and is
	// not reversible: one unmanaged resource in a domain with several eligible
	// tenants is stored as one row per tenant, and without these fields those
	// rows cannot be told from rows about different resources, so the operator
	// view that ADR 0011 grants would either double-count or merge two genuinely
	// different resources. Only the occupancy fan-out in
	// internal/service/worker.go sets them; a finding that names an allocation
	// names its resource through that allocation's binding and leaves them empty.
	ResourceType    string    `json:"resource_type,omitempty"`
	ResourceID      string    `json:"resource_id,omitempty"`
	FirstObservedAt time.Time `json:"first_observed_at"`
	LastObservedAt  time.Time `json:"last_observed_at"`
}

type Resource struct {
	AccountID string            `json:"account_id"`
	Region    string            `json:"region"`
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	CIDR      string            `json:"cidr"`
	CIDRs     []string          `json:"cidrs"`
	ParentID  string            `json:"parent_id"`
	ZoneID    string            `json:"zone_id"`
	Tags      map[string]string `json:"tags"`
	State     string            `json:"state"`
}
type Observation struct {
	DomainID   string     `json:"domain_id"`
	Generation string     `json:"generation"`
	Complete   bool       `json:"complete"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at"`
	Resources  []Resource `json:"resources"`
	Error      string     `json:"error,omitempty"`
}

// Contributor is one observed cloud resource that occupies an imported
// network's address space (ADR 0016). The onboarding import collapses every
// table row resolving to one CIDR into a single prefix, and this list is what
// that collapse used to discard: before ADR 0016 the only trace of a second
// VPC on a shared prefix was a sentence in the prefix's description, capped at
// 200 runes and editable by any operator.
//
// The JSON tags are a wire format, not a convenience. The list is written
// verbatim into the unowned NetBox custom field platform_import_contributors
// and read back from it, so changing a tag changes data already sitting in an
// inventory, not just this program's output.
//
// AssociationID and ObservedAt are pointers because ADR 0016 requires an
// explicit null and forbids guessing: a null ObservedAt is exactly what makes
// a contributor unusable as removal evidence. The empty string cannot carry
// that meaning, because it is also what a present-but-blank spreadsheet cell
// normalizes to (internal/onboard.NetworkRow's column-presence flags exist for
// the same reason).
type Contributor struct {
	Identity       string  `json:"identity"`
	AccountID      string  `json:"account_id"`
	Region         string  `json:"region"`
	Type           string  `json:"type"`
	ResourceID     string  `json:"resource_id"`
	ParentID       string  `json:"parent_id"`
	AssociationID  *string `json:"association_id"`
	ObservedAt     *string `json:"observed_at"`
	FirstSeenBatch string  `json:"first_seen_batch"`
	LastSeenBatch  string  `json:"last_seen_batch"`
	SourceFile     string  `json:"source_file"`
	SourceRow      int     `json:"source_row"`
}

type Network struct {
	ID           string
	CIDR         string
	AllocationID string
	OperationID  string
	ParentPool   bool
	// Imported reports the import tag that marks occupancy this project wrote
	// and owns nothing of. Adoption is its only reader: it is the evidence
	// that the single object being converted is the one an operator reviewed.
	Imported bool
	// Owned reports any of the inventory's ownership fields, including the
	// ones this type does not carry as values. A half-written object is owned
	// as far as adoption is concerned, and is never adoptable.
	Owned bool
	// ImportBatch names the import that wrote this occupancy. It is evidence
	// in an adoption's audit reason and nothing decides anything on it.
	ImportBatch string
	// ParentAllocationID is the parent of the allocation that owns this
	// network, and is empty for a network nothing owns. It exists because the
	// onboarding import has to tell a managed VPC from a managed subnet while
	// the inventory carries no scope: no platform custom field records one,
	// and `platform_parent_allocation_id`, which the adapter writes for every
	// managed prefix, is an exact proxy for it. A vpc request may carry no
	// parent and a subnet request must carry one
	// (internal/service.validateRequest), and an allocation is constructed in
	// exactly one place (ADR 0010), so a managed network with no parent is a
	// vpc-scoped one and nothing else. The test that keeps that true is
	// TestAVPCIsExactlyAnAllocationWithNoParent in internal/service.
	ParentAllocationID string
	// Status is the inventory's own status for this network, carried so that
	// an adoption can record what it is about to overwrite (ADR 0012,
	// AdoptionRecord.PriorStatus). Nothing decides anything on it: no rule in
	// internal/service or internal/onboard reads it, and a network the
	// inventory reports without one -- an address or a range -- leaves it
	// empty.
	Status string
	// AWSAccountID and AWSRegion are what the inventory records for this
	// network's cloud account and region -- for imported occupancy, what the
	// import wrote. Carried for the same reason as Status and read by nothing
	// that decides anything.
	AWSAccountID string
	AWSRegion    string
	// Contributors are the cloud resources the import recorded as occupying
	// this network (ADR 0016), read back from the unowned NetBox custom field.
	// A nil slice means the inventory carries no list at all -- every prefix
	// imported before ADR 0016, and every object that is not an imported
	// prefix. An empty but non-nil slice means the field holds an empty JSON
	// array, which ADR 0016 is explicit is NOT an argument that nothing
	// contributes: "it is a list nobody wrote". The two must stay
	// distinguishable, which is why this is a slice and not a count.
	//
	// Nothing in this package or in internal/service reads it: package M9b1
	// only carries it out of the inventory, and the findings that compare it
	// with a table are package M9b2's.
	Contributors []Contributor
	// ContributorsUnreadable reports that the field held something this
	// version cannot decode as a contributor list -- an operator's hand edit,
	// or a shape a later version writes. It is deliberately not an error:
	// Snapshot failing takes the whole overlap domain offline (every
	// reservation answers 503), and a malformed field on one prefix must
	// never do that. It is equally deliberately not silence: "absent" and
	// "unreadable" lead to opposite decisions, because a refresh may populate
	// an absent list, and overwriting an unreadable one would destroy
	// whatever it held. ADR 0016's rule is that UNKNOWN never frees address
	// space; this is UNKNOWN.
	ContributorsUnreadable bool
	// ContributorsReconstructed reports the top-level marker package M9b3's
	// refresh sets when it populates a contributor list on a prefix that
	// carried none at all (imported before ADR 0016 landed). It lives beside
	// the list, never inside an entry, so that a later removal (package
	// M9b4) can refuse a reconstructed list without reading any entry: the
	// list and the import that first created the prefix can never be told
	// apart again once reconstruction has happened, and ADR 0016 makes that
	// a permanent refusal rather than a warning.
	ContributorsReconstructed bool
}
type InventorySnapshot struct {
	Networks []Network
	Complete bool
}
type Inventory interface {
	Snapshot(context.Context, Domain) (InventorySnapshot, error)
	Ensure(context.Context, Allocation, string) (string, error)
	// Adopt converts the one existing unmanaged, imported network at exactly the
	// allocation's CIDR into a managed one and returns its inventory ID. It never
	// creates a network and never touches one that already belongs to something:
	// anything but a single imported, unowned object at that CIDR is a refusal,
	// and so is a re-read that does not show this allocation as the owner.
	Adopt(ctx context.Context, a Allocation, operationID string) (string, error)
	// AbandonAdoption returns the network an unfinished adoption converted to
	// the unmanaged occupancy it was, so that an adoption which can never
	// commit can be withdrawn without leaving an object claiming an allocation
	// the ledger does not hold (ADR 0012). Its evidence is this allocation's
	// own marker and nothing an operator typed: no marked network is success
	// with nothing to do, more than one is a refusal, and the single match must
	// also carry this operation's id or it belongs to something else. It
	// empties exactly the fields an adoption writes and nothing besides, keeps
	// the import tag and every field it did not write, and restores what prior
	// records -- the status, and the account and region an adoption overwrote
	// -- falling back to what an import creates where prior is empty. It never
	// deletes a network and never converts one; a re-read that still shows
	// ownership is a refusal, and so is anything it cannot decide.
	AbandonAdoption(ctx context.Context, a Allocation, operationID string, prior PriorInventory) error
	// CancelReservation deletes the network a reservation created, so that a
	// hold the platform has declared stuck can be withdrawn by the consumer
	// that owns it without leaving an object claiming an allocation the ledger
	// is about to remove (ADR 0013). It deletes where AbandonAdoption clears,
	// and the difference is not a matter of style: an adoption's network
	// belongs to an import and outlives the adoption, while a reservation's
	// belongs to nothing else, so clearing one would leave unowned, untagged
	// occupancy that refuses every later reservation at that CIDR. Its evidence
	// is this allocation's own marker and nothing a caller supplied: no marked
	// network is success with nothing to do, more than one is a refusal, and
	// the single match must also carry this operation's id and must not carry
	// the import tag, which no reservation ever writes. It deletes nothing
	// else, never by CIDR and never by a stored inventory id an uncommitted
	// hold does not have; a read-back that still finds the object is a refusal,
	// and so is anything it cannot decide.
	CancelReservation(ctx context.Context, a Allocation, operationID string) error
	Sync(context.Context, Allocation) error
	Delete(context.Context, Allocation) error
}
type Observer interface {
	Observe(context.Context, Domain) (Observation, error)
}

type State struct {
	Allocations  map[string]Allocation    `json:"allocations"`
	Operations   map[string]Operation     `json:"operations"`
	Requests     map[string]Idempotency   `json:"requests"`
	Observations map[string][]Observation `json:"observations"`
	Findings     map[string]Finding       `json:"findings"`
	Events       []Event                  `json:"events"`
	Coverage     map[string]string        `json:"coverage"`
}

func NewState() *State {
	return &State{Allocations: map[string]Allocation{}, Operations: map[string]Operation{}, Requests: map[string]Idempotency{}, Observations: map[string][]Observation{}, Findings: map[string]Finding{}, Coverage: map[string]string{}}
}

type Ledger interface {
	View(context.Context, func(*State) error) error
	Update(context.Context, func(*State) error) error
	Ready(context.Context) error
	// Events returns one allocation's full audit history, ordered by when
	// each event happened (At ascending), with the event id as a
	// deterministic tiebreaker for two events recorded at the same instant.
	// It exists because package M4f stopped View, like Update before it
	// (package M4e), from decoding audit_events on every transaction: every
	// production writer of State.Events only appends and never reads it
	// back (M4e's survey of every ".Events" site), and no transport
	// endpoint or CLI verb lists an allocation's history today, so the
	// narrowest honest read path for the few callers that DO want history
	// -- the append-only proof, and the two tests that check an audit trail
	// outlives the allocation it names -- is a keyed, additive method
	// rather than a State that carries every event whether a caller asked
	// for it or not. It is additive to the port: every existing Ledger
	// double satisfies it automatically by embedding domain.Ledger.
	Events(ctx context.Context, allocationID string) ([]Event, error)
}

func NewID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}
