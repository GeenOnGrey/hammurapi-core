// Package testutil has helpers shared by unit tests.
package testutil

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GeenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GeenOnGrey/hammurapi-core/internal/specdata/mocks"
)

// User builds a principal from grants like "editor:product" or "global".
func User(grants ...string) *domain.Principal {
	p := domain.NewPrincipal(uuid.New(), "user", "Test User", false)
	for _, g := range grants {
		if g == "global" {
			p.GlobalAdmin = true
			continue
		}
		role, area, _ := strings.Cut(g, ":")
		p.Grant(domain.Role(role), domain.Area(area))
	}
	return p
}

// Feature builds an in-progress feature FMS.CAR-0005.
func Feature(approvalRequired bool) *specdata.Feature {
	return &specdata.Feature{
		ID: uuid.New(), UniqueID: "FMS.CAR-0005", SystemID: uuid.New(), DomainKey: "FMS", SystemKey: "CAR",
		ApprovalRequired: approvalRequired, Number: 5, Title: "Onboarding", Branch: "feature/FMS.CAR-0005",
		PRNumber: 42, PRURL: "https://git.example/mr/42", Status: domain.FeatureInProgress, CreatedAt: time.Now(),
	}
}

// Gate builds an active gate.
func Gate(f *specdata.Feature, area domain.Area, status domain.GateStatus) *specdata.Gate {
	return &specdata.Gate{ID: uuid.New(), FeatureID: f.ID, Area: area, Status: status, HeadCommit: "sha-" + string(area), CreatedAt: time.Now()}
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
