package approvals

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	emocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/events/mocks"
	gmocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GeenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

type fixture struct {
	store  *smocks.MockStore
	git    *gmocks.MockProvider
	tokens *gmocks.MockTokenSource
	ev     *emocks.MockPublisher
	svc    *Service
}

func setup(t *testing.T) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), git: gmocks.NewMockProvider(ctrl), tokens: gmocks.NewMockTokenSource(ctrl), ev: emocks.NewMockPublisher(ctrl)}
	f.svc = NewService(f.store, nil, f.git, f.tokens, f.ev)
	f.tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	f.ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	tu.PassThroughTx(f.store)
	return f
}

func (f *fixture) feature(feat *specdata.Feature, gates ...*specdata.Gate) {
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		g := g
		all = append(all, *g)
		f.store.EXPECT().ActiveGate(gomock.Any(), feat.ID, g.Area).Return(g, nil).AnyTimes()
	}
	f.store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
}

// APPR-03: design cannot be approved while product is not approved.
func TestApproveRequiresPreviousGates(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.feature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateInReview), tu.Gate(feat, domain.AreaDesign, domain.GateInReview))
	_, err := f.svc.Approve(context.Background(), tu.User("approver:design"), feat.UniqueID, domain.AreaDesign)
	tu.Code(t, err, 409, "previous_not_approved")
}

// APPR-04: a draft cannot be approved.
func TestApproveDraft(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.feature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	_, err := f.svc.Approve(context.Background(), tu.User("approver:product"), feat.UniqueID, domain.AreaProduct)
	tu.Code(t, err, 409, "not_in_review")
}

// APPR-06 / ROLE-02: an editor of product who approves arch cannot approve product.
func TestApproveWithoutApproverRole(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Approve(context.Background(), tu.User("editor:product", "approver:arch"), "FMS.CAR-0005", domain.AreaProduct)
	tu.Code(t, err, 403, "forbidden")
}

// APPR-05: git has a newer commit than the projection → rejected, nothing written.
func TestApproveStaleDocument(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	g := tu.Gate(feat, domain.AreaProduct, domain.GateInReview)
	f.feature(feat, g)
	f.git.EXPECT().LatestCommit(gomock.Any(), "tok", feat.Branch, "specs/FMS/CAR/FMS.CAR-0005/product").Return("newer", nil)
	_, err := f.svc.Approve(context.Background(), tu.User("approver:product"), feat.UniqueID, domain.AreaProduct)
	tu.Code(t, err, 409, "document_changed")
}

// APPR-02: approval records the approved commit.
func TestApproveSetsApprovedCommit(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	g := tu.Gate(feat, domain.AreaProduct, domain.GateInReview)
	f.feature(feat, g)
	f.git.EXPECT().LatestCommit(gomock.Any(), "tok", feat.Branch, gomock.Any()).Return(g.HeadCommit, nil)
	f.store.EXPECT().SaveGate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, sg *specdata.Gate) error {
		if sg.Status != domain.GateApproved || sg.ApprovedCommit == nil || *sg.ApprovedCommit != g.HeadCommit {
			t.Fatalf("unexpected gate %+v", sg)
		}
		return nil
	})
	f.store.EXPECT().InsertEvent(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, e *specdata.GateEvent) error {
		if e.Type != domain.EventApproved {
			t.Fatalf("event %s", e.Type)
		}
		return nil
	})
	if _, err := f.svc.Approve(context.Background(), tu.User("approver:product"), feat.UniqueID, domain.AreaProduct); err != nil {
		t.Fatal(err)
	}
}

// NOAPR-06: submit is refused in a domain without approval.
func TestSubmitInDomainWithoutApproval(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(false)
	f.feature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	_, err := f.svc.Submit(context.Background(), tu.User("editor:product"), feat.UniqueID, domain.AreaProduct)
	tu.Code(t, err, 409, "approval_disabled")
}

// APPR-01: submit moves draft to in_review and sets submitted_at.
func TestSubmit(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.feature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.store.EXPECT().SaveGate(gomock.Any(), gomock.Any()).Return(nil)
	f.store.EXPECT().InsertEvent(gomock.Any(), gomock.Any()).Return(nil)
	g, err := f.svc.Submit(context.Background(), tu.User("editor:product"), feat.UniqueID, domain.AreaProduct)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != domain.GateInReview || g.SubmittedAt == nil {
		t.Fatalf("got %+v", g)
	}
}

// HOME-02: users without approver roles get an empty queue.
func TestListWithoutApproverRole(t *testing.T) {
	f := setup(t)
	l, err := f.svc.List(context.Background(), tu.User("editor:product"), pageOf())
	if err != nil || len(l.Items) != 0 {
		t.Fatalf("got %v %v", l, err)
	}
}

func pageOf() httpx.Page { return httpx.Page{Limit: 50} }
