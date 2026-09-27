package handoff

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	emocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/events/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GeenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

func setup(t *testing.T, feat *specdata.Feature, gates ...*specdata.Gate) (*smocks.MockStore, *gmocks.MockProvider, *Service) {
	ctrl := gomock.NewController(t)
	store, prov := smocks.NewMockStore(ctrl), gmocks.NewMockProvider(ctrl)
	tokens := gmocks.NewMockTokenSource(ctrl)
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	tu.PassThroughTx(store)
	store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		all = append(all, *g)
	}
	store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
	store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(nil, nil).AnyTimes()
	return store, prov, NewService(store, prov, tokens, ev)
}

// HAND-02
func TestHandoffWithUnapprovedGate(t *testing.T) {
	feat := tu.Feature(true)
	_, _, svc := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved), tu.Gate(feat, domain.AreaDesign, domain.GateInReview))
	err := svc.Handoff(context.Background(), tu.User("editor:product"), feat.UniqueID)
	tu.Code(t, err, 409, "not_all_approved")
}

// HAND-03
func TestHandoffMerges(t *testing.T) {
	feat := tu.Feature(true)
	store, prov, svc := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	prov.EXPECT().MergePR(gomock.Any(), "tok", feat.PRNumber, gomock.Any()).Return(nil)
	store.EXPECT().MarkHandedOff(gomock.Any(), feat.ID, gomock.Any(), false).Return(nil)
	store.EXPECT().DropLock(gomock.Any(), feat.ID).Return(nil)
	if err := svc.Handoff(context.Background(), tu.User("approver:product"), feat.UniqueID); err != nil {
		t.Fatal(err)
	}
}

// NOAPR-05: without approval the feature is handed off at once and marked.
func TestHandoffWithoutApproval(t *testing.T) {
	feat := tu.Feature(false)
	store, prov, svc := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	prov.EXPECT().MergePR(gomock.Any(), "tok", feat.PRNumber, gomock.Any()).Return(nil)
	store.EXPECT().MarkHandedOff(gomock.Any(), feat.ID, gomock.Any(), true).Return(nil)
	store.EXPECT().DropLock(gomock.Any(), feat.ID).Return(nil)
	if err := svc.Handoff(context.Background(), tu.User("editor:product"), feat.UniqueID); err != nil {
		t.Fatal(err)
	}
}

// HAND-04: the provider refuses the merge → 422 with the reason, status unchanged.
func TestHandoffProviderRefuses(t *testing.T) {
	feat := tu.Feature(true)
	_, prov, svc := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	prov.EXPECT().MergePR(gomock.Any(), "tok", feat.PRNumber, gomock.Any()).
		Return(&git.APIError{Provider: "gitlab", Status: 405, Message: "branch is protected"})
	err := svc.Handoff(context.Background(), tu.User("editor:product"), feat.UniqueID)
	tu.Code(t, err, 422, "provider_refused")
}

func TestHandoffRequiresRoleInFeature(t *testing.T) {
	feat := tu.Feature(true)
	_, _, svc := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	err := svc.Handoff(context.Background(), tu.User("editor:qa"), feat.UniqueID)
	tu.Code(t, err, 403, "forbidden")
}
