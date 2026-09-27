package specdata

import (
	"time"

	"github.com/google/uuid"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
)

// GateDTO is the API representation of a gate.
type GateDTO struct {
	Area           domain.Area       `json:"area"`
	Status         domain.GateStatus `json:"status"`
	HeadCommit     string            `json:"headCommit"`
	SubmittedAt    *time.Time        `json:"submittedAt"`
	ApprovedCommit *string           `json:"approvedCommit"`
	ApprovedBy     *string           `json:"approvedBy"`
	ApprovedAt     *time.Time        `json:"approvedAt"`
	CreatedAt      time.Time         `json:"createdAt"`
}

// ToGateDTO converts a gate.
func ToGateDTO(g Gate) GateDTO {
	return GateDTO{Area: g.Area, Status: g.Status, HeadCommit: g.HeadCommit, SubmittedAt: g.SubmittedAt,
		ApprovedCommit: g.ApprovedCommit, ApprovedBy: g.ApprovedByName, ApprovedAt: g.ApprovedAt, CreatedAt: g.CreatedAt}
}

// GateDTOs converts gates.
func GateDTOs(gs []Gate) []GateDTO {
	out := make([]GateDTO, 0, len(gs))
	for _, g := range gs {
		out = append(out, ToGateDTO(g))
	}
	return out
}

// LockDTO is the API representation of a lock.
type LockDTO struct {
	UserID    uuid.UUID `json:"userId"`
	UserName  string    `json:"userName"`
	LockedAt  time.Time `json:"lockedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ToLockDTO converts a lock (nil-safe).
func ToLockDTO(l *Lock) *LockDTO {
	if l == nil {
		return nil
	}
	return &LockDTO{UserID: l.LockedBy, UserName: l.LockedByName, LockedAt: l.LockedAt, ExpiresAt: l.ExpiresAt}
}

// FeatureSummary is a feature in lists.
type FeatureSummary struct {
	UniqueID         string               `json:"uniqueId"`
	Domain           string               `json:"domain"`
	System           string               `json:"system"`
	Title            string               `json:"title"`
	Status           domain.FeatureStatus `json:"status"`
	ApprovalRequired bool                 `json:"approvalRequired"`
	Parent           *string              `json:"parent"`
	CreatedAt        time.Time            `json:"createdAt"`
	Gates            []GateDTO            `json:"gates"`
}

// ToSummary converts a listed feature.
func ToSummary(f ListedFeature) FeatureSummary {
	return FeatureSummary{UniqueID: f.UniqueID, Domain: f.DomainKey, System: f.SystemKey, Title: f.Title, Status: f.Status,
		ApprovalRequired: f.ApprovalRequired, Parent: f.ParentUniqueID, CreatedAt: f.CreatedAt, Gates: GateDTOs(f.Gates)}
}

// AllApproved reports whether every gate is approved.
func AllApproved(gs []Gate) bool {
	for _, g := range gs {
		if g.Status != domain.GateApproved {
			return false
		}
	}
	return true
}

// HasAnyRoleIn reports whether p is an editor or approver in any of the gates' areas.
func HasAnyRoleIn(p *domain.Principal, gs []Gate) bool {
	for _, g := range gs {
		if p.Has(domain.RoleEditor, g.Area) || p.Has(domain.RoleApprover, g.Area) {
			return true
		}
	}
	return false
}

// EditorOfAll reports whether p is an editor in every gate's area.
func EditorOfAll(p *domain.Principal, gs []Gate) bool {
	for _, g := range gs {
		if !p.Has(domain.RoleEditor, g.Area) {
			return false
		}
	}
	return true
}
