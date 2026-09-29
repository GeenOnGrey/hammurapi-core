package gates

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	emocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/events/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GeenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

type fakeGen struct {
	areas   []domain.Area
	comment string
}

func (g *fakeGen) Generate(_ context.Context, _ *specdata.Feature, areas []domain.Area, _ uuid.UUID, comment string) error {
	g.areas, g.comment = areas, comment
	return nil
}

type fixture struct {
	store *smocks.MockStore
	git   *gmocks.MockProvider
	gen   *fakeGen
	svc   *Service
}

func setup(t *testing.T, feat *specdata.Feature, gates ...*specdata.Gate) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), git: gmocks.NewMockProvider(ctrl)}
	tokens := gmocks.NewMockTokenSource(ctrl)
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	f.gen = &fakeGen{}
	f.svc = NewService(f.store, f.git, tokens, ev, f.gen, "main")
	tu.PassThroughTx(f.store)
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		all = append(all, *g)
		f.store.EXPECT().ActiveGate(gomock.Any(), feat.ID, g.Area).Return(g, nil).AnyTimes()
	}
	f.store.EXPECT().ActiveGate(gomock.Any(), feat.ID, gomock.Any()).Return(nil, specdata.ErrNotFound).AnyTimes()
	f.store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
	return f
}

// ROLE-02 / ROLE-03: only experts of the feature's domain edit gates.
func TestSaveRequiresDomainExpert(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaDesign, domain.GateDraft))
	for _, u := range []string{"expert:PAY:product", "admin:design"} {
		_, err := f.svc.SaveDocument(context.Background(), tu.User(u), feat.UniqueID, domain.AreaDesign, SaveInput{Content: "x"}, false)
		tu.Code(t, err, 403, "forbidden")
	}
}

// GEN-03: tech and qa are not edited by hand.
func TestSaveGeneratedGate(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaTech, domain.GateDraft))
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaTech, SaveInput{Content: "x"}, false)
	tu.Code(t, err, 403, "gate_generated")
}

// GEN-04 / regenerate: needs approved human gates in approval domains, passes the chat comment.
func TestRegenerate(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateInReview))
	err := f.svc.Regenerate(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaTech, "")
	tu.Code(t, err, 409, "gates_not_approved")

	f = setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	if err := f.svc.Regenerate(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaQA, "cover R2"); err != nil {
		t.Fatal(err)
	}
	if len(f.gen.areas) != 1 || f.gen.areas[0] != domain.AreaQA || f.gen.comment != "cover R2" {
		t.Fatalf("generator got %+v", f.gen)
	}
}

// LOCK-02: PUT from another user while the feature is locked → 423.
func TestSaveLockedByOther(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{LockedBy: uuid.New(), LockedByName: "Anna"}, false, nil)
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"}, false)
	tu.Code(t, err, 423, "feature_locked")
}

func (f *fixture) lockAndFile(feat *specdata.Feature, content string) {
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{}, true, nil)
	f.git.EXPECT().GetFile(gomock.Any(), "tok", feat.Branch, "specs/FMS/CAR/FTR.FMS.CAR-0005/product/spec.md").
		Return(&git.File{Content: []byte(content)}, nil)
}

// GATE-05: a stale baseSha is refused and nothing is committed.
func TestSaveStaleBase(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.lockAndFile(feat, "# current\n")
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# mine\n", BaseSHA: git.BlobSHA([]byte("# old\n"))}, false)
	tu.Code(t, err, 409, "stale_document")
}

// GATE-09: saving unchanged content creates no commit.
func TestSaveUnchangedIsNoop(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.lockAndFile(feat, "# same\n")
	res, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# same\n", BaseSHA: git.BlobSHA([]byte("# same\n"))}, false)
	if err != nil || res.Commit != nil {
		t.Fatalf("got %+v %v", res, err)
	}
}

// GATE-01 / HOOK-04: a save commits with feature/area trailers, and the agent flag for agent edits.
func TestSaveCommitsWithTrailers(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateInReview))
	f.lockAndFile(feat, "# old\n")
	f.git.EXPECT().Commit(gomock.Any(), "tok", feat.Branch, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _, msg string, _ []git.FileChange) (string, error) {
			tr := git.ParseTrailers(msg)
			if tr.Feature != feat.UniqueID || tr.Area != "product" || !tr.Agent {
				t.Fatalf("trailers %+v", tr)
			}
			return "c2", nil
		})
	res, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# new\n"}, true)
	if err != nil || res.Commit == nil || *res.Commit != "c2" {
		t.Fatalf("got %+v %v", res, err)
	}
}

// CHAT-04: the agent does not edit an approved gate.
func TestAgentCannotEditApproved(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"}, true)
	tu.Code(t, err, 409, "gate_approved")
}

// CG-03: specifications are read-only during code generation.
func TestSaveDuringCodegen(t *testing.T) {
	feat := tu.Feature(true)
	feat.Phase = domain.PhaseCodegen
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"}, false)
	tu.Code(t, err, 409, "codegen_in_progress")
}

// DEL-03: the product gate cannot be deleted.
func TestDeleteLastGate(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	err := f.svc.DeleteGate(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct)
	tu.Code(t, err, 409, "last_gate")
}

// GEN-03: a generated gate cannot be deleted.
func TestDeleteGeneratedGate(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft), tu.Gate(feat, domain.AreaTech, domain.GateDraft))
	err := f.svc.DeleteGate(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaTech)
	tu.Code(t, err, 403, "gate_generated")
}

// DEL-01: deletion commits removal of the whole folder with the delete trailer
// and records the delete commit as the gate's head (so the webhook skips it).
func TestDeleteGate(t *testing.T) {
	feat := tu.Feature(true)
	design := tu.Gate(feat, domain.AreaDesign, domain.GateInReview)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved), design)
	f.store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(nil, nil)
	dir := "specs/FMS/CAR/FTR.FMS.CAR-0005/design"
	f.git.EXPECT().ListFiles(gomock.Any(), "tok", feat.Branch, dir).Return([]string{dir + "/spec.md", dir + "/mock.png"}, nil)
	f.git.EXPECT().Commit(gomock.Any(), "tok", feat.Branch, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _, msg string, ch []git.FileChange) (string, error) {
			if git.ParseTrailers(msg).Delete != "design" || len(ch) != 2 || !ch[0].Delete || !ch[1].Delete {
				t.Fatalf("msg %q changes %+v", msg, ch)
			}
			return "del", nil
		})
	f.store.EXPECT().SaveGate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, g *specdata.Gate) error {
		if g.DeletedAt == nil || g.HeadCommit != "del" {
			t.Fatalf("gate %+v", g)
		}
		return nil
	})
	f.store.EXPECT().InsertEvent(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, e *specdata.GateEvent) error {
		if e.Type != domain.EventDeleted {
			t.Fatalf("event %s", e.Type)
		}
		return nil
	})
	if err := f.svc.DeleteGate(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaDesign); err != nil {
		t.Fatal(err)
	}
}
