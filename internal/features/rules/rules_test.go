package rules

import (
	"context"
	"testing"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

// ROLE-04 / ROLE-08: only admins of an area change its rules; a global admin only reads.
func TestAccess(t *testing.T) {
	design := tu.User("admin:design")
	if !CanChange(design, domain.AreaDesign) || CanChange(design, domain.AreaQA) || CanView(design, domain.AreaQA) {
		t.Fatal("area admin access")
	}
	global := tu.User("global")
	if !CanView(global, domain.AreaQA) || CanChange(global, domain.AreaQA) {
		t.Fatal("global admin access")
	}
	s := &Service{}
	_, err := s.Propose(context.Background(), design, domain.AreaQA, ProposeInput{File: FileTemplate})
	tu.Code(t, err, 403, "forbidden")
	_, err = s.Propose(context.Background(), global, domain.AreaQA, ProposeInput{File: FileTemplate})
	tu.Code(t, err, 403, "forbidden")
}
