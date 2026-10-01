// Package testutil has helpers shared by unit tests.
package testutil

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GreenOnGrey/hammurapi-core/internal/specdata/mocks"
)

// User builds a principal from grants:
//
//	"global"                 global administrator
//	"admin:<area>"           area administrator
//	"expert:<DOMAIN>:<kind>" product or technical expert of a domain
//	"owner:<service>"        owner of a service
func User(grants ...string) *domain.Principal {
	p := domain.NewPrincipal(uuid.New(), "user", "Test User", false)
	for _, g := range grants {
		parts := strings.Split(g, ":")
		switch parts[0] {
		case "global":
			p.GlobalAdmin = true
		case "admin":
			p.GrantAreaAdmin(domain.Area(parts[1]))
		case "expert":
			p.GrantExpert(parts[1], domain.ExpertKind(parts[2]))
		case "owner":
			p.GrantOwner(parts[1])
		}
	}
	return p
}

// Feature builds a feature FTR.FMS.CAR-0005 in the spec phase.
func Feature(approvalRequired bool) *specdata.Feature {
	return &specdata.Feature{
		ID: uuid.New(), UniqueID: "FTR.FMS.CAR-0005", SystemID: uuid.New(), DomainKey: "FMS", SystemKey: "CAR",
		ApprovalRequired: approvalRequired, Number: 5, Title: "Onboarding", Branch: "feature/FTR.FMS.CAR-0005",
		PRNumber: 42, PRURL: "https://git.example/mr/42", Phase: domain.PhaseSpec, CreatedAt: time.Now(),
	}
}

// Gate builds an active gate.
func Gate(f *specdata.Feature, area domain.Area, status domain.GateStatus) *specdata.Gate {
	return &specdata.Gate{ID: uuid.New(), FeatureID: f.ID, Area: area, Status: status, Generated: area.Generated(), HeadCommit: "sha-" + string(area), CreatedAt: time.Now()}
}

// PassThroughTx makes store.InTx run the callback with the same mock.
func PassThroughTx(s *smocks.MockStore) {
	s.EXPECT().InTx(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(_ context.Context, fn func(specdata.Store) error) error { return fn(s) })
}

// Code asserts the API error code (and HTTP status) of err.
func Code(t *testing.T, err error, status int, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("want %d %s, got %v", status, code, err)
	}
	if e.Status != status || (code != "" && e.Code != code) {
		t.Fatalf("want %d %s, got %d %s (%s)", status, code, e.Status, e.Code, e.Message)
	}
}
