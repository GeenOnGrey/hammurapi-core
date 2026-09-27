// Package domain holds the core vocabulary shared by all feature slices:
// areas, roles, statuses and the authenticated principal.
package domain

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/google/uuid"
)

// Area is a specification area; it is also a gate.
type Area string

const (
	AreaProduct Area = "product"
	AreaDesign  Area = "design"
	AreaArch    Area = "arch"
	AreaTech    Area = "tech"
	AreaQA      Area = "qa"
)

// Areas lists all areas in the fixed approval order.
var Areas = []Area{AreaProduct, AreaDesign, AreaArch, AreaTech, AreaQA}

// Index returns the position of the area in approval order, or -1.
func (a Area) Index() int {
	for i, x := range Areas {
		if x == a {
			return i
		}
	}
	return -1
}

// Valid reports whether a is a known area.
func (a Area) Valid() bool { return a.Index() >= 0 }

// ParseArea validates an area string.
func ParseArea(s string) (Area, error) {
	a := Area(s)
	if !a.Valid() {
		return "", fmt.Errorf("unknown area %q", s)
	}
	return a, nil
}

// SortAreas orders areas by approval order.
func SortAreas(as []Area) {
	sort.Slice(as, func(i, j int) bool { return as[i].Index() < as[j].Index() })
}

// Role is an application role; every role is granted per area.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleApprover Role = "approver"
	RoleEditor   Role = "editor"
)

// Roles lists all roles.
var Roles = []Role{RoleEditor, RoleApprover, RoleAdmin}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleApprover || r == RoleEditor }

// FeatureStatus is the lifecycle status of a feature.
type FeatureStatus string

const (
	FeatureInProgress FeatureStatus = "in_progress"
	FeatureHandedOff  FeatureStatus = "handed_off"
	FeatureDeleted    FeatureStatus = "deleted"
)

// GateStatus is the status of a gate.
type GateStatus string

const (
	GateDraft    GateStatus = "draft"
	GateInReview GateStatus = "in_review"
	GateApproved GateStatus = "approved"
)

// GateEventType is a gate history event.
type GateEventType string

const (
	EventCreated   GateEventType = "created"
	EventEdited    GateEventType = "edited"
	EventSubmitted GateEventType = "submitted"
	EventApproved  GateEventType = "approved"
	EventReset     GateEventType = "reset"
	EventDeleted   GateEventType = "deleted"
)

// AgentTone is the communication tone of the user's agent.
type AgentTone string

const (
	ToneBusiness AgentTone = "business"
	ToneFriendly AgentTone = "friendly"
	ToneConcise  AgentTone = "concise"
	ToneMentor   AgentTone = "mentor"
)

// Tones lists all agent tones.
var Tones = []AgentTone{ToneBusiness, ToneFriendly, ToneConcise, ToneMentor}

// Valid reports whether t is a known tone.
func (t AgentTone) Valid() bool {
	for _, x := range Tones {
		if x == t {
			return true
		}
	}
	return false
}

// Languages supported by the interface; the first is the fallback default.
var Languages = []string{"en", "ru", "de", "es", "zh-CN"}

// ValidLanguage reports whether lang is supported.
func ValidLanguage(lang string) bool {
	for _, l := range Languages {
		if l == lang {
			return true
		}
	}
	return false
}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// ValidKey reports whether s is a valid domain or system key.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

var uniqueIDRe = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,9})\.([A-Z][A-Z0-9]{1,9})-(\d{4,})$`)

// ParseUniqueID splits FMS.CAR-0005 into domain, system and number.
func ParseUniqueID(s string) (domainKey, systemKey string, number int, ok bool) {
	m := uniqueIDRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, false
	}
	fmt.Sscanf(m[3], "%d", &number)
	return m[1], m[2], number, true
}

// FormatUniqueID builds FMS.CAR-0005.
func FormatUniqueID(domainKey, systemKey string, number int) string {
	return fmt.Sprintf("%s.%s-%04d", domainKey, systemKey, number)
}

// Principal is the authenticated user with their roles, loaded per request.
type Principal struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	GlobalAdmin bool
	Roles       map[Role]map[Area]bool
}

// NewPrincipal builds an empty principal.
func NewPrincipal(id uuid.UUID, username, displayName string, globalAdmin bool) *Principal {
	return &Principal{UserID: id, Username: username, DisplayName: displayName, GlobalAdmin: globalAdmin, Roles: map[Role]map[Area]bool{}}
}

// Grant adds a role in an area.
func (p *Principal) Grant(r Role, a Area) {
	if p.Roles == nil {
		p.Roles = map[Role]map[Area]bool{}
	}
	if p.Roles[r] == nil {
		p.Roles[r] = map[Area]bool{}
	}
	p.Roles[r][a] = true
}

// Has reports whether the principal holds role r in area a.
func (p *Principal) Has(r Role, a Area) bool { return p != nil && p.Roles[r][a] }

// HasRole reports whether the principal holds role r in any area.
func (p *Principal) HasRole(r Role) bool { return p != nil && len(p.Roles[r]) > 0 }

// AreasFor lists areas where the principal holds role r, in approval order.
func (p *Principal) AreasFor(r Role) []Area {
	var out []Area
	for _, a := range Areas {
		if p.Has(r, a) {
			out = append(out, a)
		}
	}
	return out
}

// IsAnyAdmin reports whether the principal is a global admin or an admin of any area.
func (p *Principal) IsAnyAdmin() bool { return p != nil && (p.GlobalAdmin || p.HasRole(RoleAdmin)) }
