package domains

import (
	"context"
	"testing"

	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

// These checks run before any database access, so the service has no repository.

// ROLE-10 / NOAPR-12: non-admins cannot change the dictionary or approval settings.
func TestNonAdminRejected(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	p := tu.User("editor:product")
	tu.Code(t, s.CreateDomain(ctx, p, "FMS", "Fleet", true), 403, "forbidden")
	yes := false
	tu.Code(t, s.PatchDomain(ctx, p, "FMS", nil, &yes), 403, "forbidden")
	_, err := s.SetApprovalAll(ctx, p, false)
	tu.Code(t, err, 403, "forbidden")
}

// ADM-04: keys are uppercase latin; checked for any admin (ROLE-09: area admins may create).
func TestKeyValidation(t *testing.T) {
	s := &Service{}
	for _, key := range []string{"fms", "F", "1AB", "TOOLONGKEY1"} {
		tu.Code(t, s.CreateDomain(context.Background(), tu.User("admin:qa"), key, "x", true), 422, "invalid_key")
	}
}
