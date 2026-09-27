// Package specdata is the shared projection of features, gates, gate history
// and feature locks. Several slices (features, gates, approvals, handoff,
// webhooks, imports, agent) read and write the same rows, so the repository
// lives here instead of being duplicated per slice.
package specdata

//go:generate go tool mockgen -destination=mocks/store.go -package=mocks . Store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// LockTTL is the lifetime of a feature edit lock, extended by editor activity.
const LockTTL = 15 * time.Minute

// System is a system with its domain.
type System struct {
	ID               uuid.UUID
	DomainID         uuid.UUID
	DomainKey        string
	Key              string
	Name             string
	ApprovalRequired bool
}

// Feature is a feature row joined with its system, domain and user names.
type Feature struct {
	ID                       uuid.UUID
	UniqueID                 string
	SystemID                 uuid.UUID
	DomainKey                string
	SystemKey                string
	ApprovalRequired         bool
	Number                   int
	Title                    string
	Branch                   string
	PRNumber                 int
	PRURL                    string
	Status                   domain.FeatureStatus
	ParentID                 *uuid.UUID
	ParentUniqueID           *string
	CreatedBy                uuid.UUID
	CreatedByName            string
	CreatedAt                time.Time
	HandedOffBy              *uuid.UUID
	HandedOffByName          *string
	HandedOffAt              *time.Time
	HandedOffWithoutApproval bool
	DeletedBy                *uuid.UUID
	DeletedByName            *string
	DeletedAt                *time.Time
	BranchCleanupPending     bool
}

// IsFix reports whether the feature is a fix feature.
func (f *Feature) IsFix() bool { return f.ParentID != nil }

// Gate is a gate row.
type Gate struct {
	ID             uuid.UUID
	FeatureID      uuid.UUID
	Area           domain.Area
	Status         domain.GateStatus
	HeadCommit     string
	SubmittedAt    *time.Time
	ApprovedCommit *string
	ApprovedBy     *uuid.UUID
	ApprovedByName *string
	ApprovedAt     *time.Time
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
	DeletedBy      *uuid.UUID
	DeletedAt      *time.Time
}

// GateEvent is a gate history entry.
type GateEvent struct {
	ID        uuid.UUID
	GateID    uuid.UUID
	Area      domain.Area
	Type      domain.GateEventType
	ActorID   *uuid.UUID
	ActorName *string
	IsAgent   bool
	CommitSHA *string
	CreatedAt time.Time
}

// Lock is a feature edit lock.
type Lock struct {
	FeatureID    uuid.UUID
	LockedBy     uuid.UUID
	LockedByName string
	LockedAt     time.Time
	ExpiresAt    time.Time
}

// FeatureRef is a short reference to a feature.
type FeatureRef struct {
	UniqueID string               `json:"uniqueId"`
	Title    string               `json:"title"`
	Status   domain.FeatureStatus `json:"status"`
}

// ListFilter filters the home page feature list.
type ListFilter struct {
	UserID uuid.UUID
	Domain string // "mine", "all" or a domain key
	Status string // "in_progress", "handed_off", "all"
	Query  string
	Page   httpx.Page
}

// ListedFeature is a row of the feature list with its gates.
type ListedFeature struct {
	Feature
	Gates []Gate
}

// Store is the repository of the spec projection.
type Store interface {
	// InTx runs fn with a transactional store.
	InTx(ctx context.Context, fn func(Store) error) error

	SystemByKeys(ctx context.Context, domainKey, systemKey string) (*System, error)
	// NextNumber increments the system counter; the row stays locked until the transaction ends.
	NextNumber(ctx context.Context, systemID uuid.UUID) (int, error)
	// PeekNumber returns the last issued number without reserving.
	PeekNumber(ctx context.Context, systemID uuid.UUID) (int, error)

	FeatureByUniqueID(ctx context.Context, uniqueID string) (*Feature, error)
	FeatureByID(ctx context.Context, id uuid.UUID) (*Feature, error)
	InsertFeature(ctx context.Context, f *Feature) error
	MarkHandedOff(ctx context.Context, id, by uuid.UUID, withoutApproval bool) error
	MarkDeleted(ctx context.Context, id, by uuid.UUID, cleanupPending bool) error
	SetBranchCleanupPending(ctx context.Context, id uuid.UUID, pending bool) error
	FeaturesPendingCleanup(ctx context.Context) ([]Feature, error)
	Fixes(ctx context.Context, parentID uuid.UUID) ([]FeatureRef, error)
	ListFeatures(ctx context.Context, f ListFilter) ([]ListedFeature, error)

	ActiveGates(ctx context.Context, featureID uuid.UUID) ([]Gate, error)
	ActiveGate(ctx context.Context, featureID uuid.UUID, area domain.Area) (*Gate, error)
	InsertGate(ctx context.Context, g *Gate) error
	SaveGate(ctx context.Context, g *Gate) error
	InsertEvent(ctx context.Context, e *GateEvent) error
	History(ctx context.Context, featureID uuid.UUID, area domain.Area, page httpx.Page) ([]GateEvent, error)
	LastSubmitter(ctx context.Context, gateID uuid.UUID) (*uuid.UUID, *string, error)

	GetLock(ctx context.Context, featureID uuid.UUID) (*Lock, error)
	// AcquireLock takes or extends the lock; if another user holds a live lock it
	// returns that lock and acquired=false.
	AcquireLock(ctx context.Context, featureID, userID uuid.UUID) (lock *Lock, acquired bool, err error)
	ReleaseLock(ctx context.Context, featureID, userID uuid.UUID) error
	DropLock(ctx context.Context, featureID uuid.UUID) error

	UserIDByUsername(ctx context.Context, username string) (*uuid.UUID, error)
	// MarkWebhookProcessed records a provider event id; false if it was already processed.
	MarkWebhookProcessed(ctx context.Context, eventID string) (bool, error)
	// SetImportItem records the outcome of one imported feature.
	SetImportItem(ctx context.Context, importID uuid.UUID, archiveID, status string, featureID *uuid.UUID, errText *string) error
}
