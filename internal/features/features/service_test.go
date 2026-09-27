package features

import (
	"context"
	"errors"
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

type fixture struct {
	store *smocks.MockStore
	git   *gmocks.MockProvider
	svc   *Service
}

func setup(t *testing.T) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), git: gmocks.NewMockProvider(ctrl)}
	tokens := gmocks.NewMockTokenSource(ctrl)
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	f.svc = NewService(f.store, f.git, tokens, ev, "main")
	return f
}

// FEAT-04
func TestCreateRequiresProductEditor(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Create(context.Background(), tu.User("editor:design"), CreateInput{Domain: "FMS", System: "CAR", Title: "X"})
	tu.Code(t, err, 403, "forbidden")
}

// FEAT-05
func TestCreateUnknownSystem(t *testing.T) {
	f := setup(t)
	f.store.EXPECT().SystemByKeys(gomock.Any(), "FMS", "NOPE").Return(nil, specdata.ErrNotFound)
	_, err := f.svc.Create(context.Background(), tu.User("editor:product"), CreateInput{Domain: "FMS", System: "NOPE", Title: "X"})
	tu.Code(t, err, 422, "unknown_system")
}

// FIX-02: a fix needs a handed-off parent.
func TestCreateFixOfFeatureInProgress(t *testing.T) {
	f := setup(t)
	parent := tu.Feature(true)
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), parent.UniqueID).Return(parent, nil)
	uid := parent.UniqueID
	_, err := f.svc.Create(context.Background(), tu.User("editor:product"), CreateInput{Title: "Fix", Parent: &uid})
	tu.Code(t, err, 422, "parent_not_handed_off")
}

// FEAT-01 / FEAT-03: the happy path commits the template to the new branch; a
// failing commit deletes the branch and rolls the transaction (and number) back.
func TestCreate(t *testing.T) {
	sys := &specdata.System{ID: uuid.New(), DomainKey: "FMS", Key: "CAR", ApprovalRequired: true}
	for _, commitFails := range []bool{false, true} {
		f := setup(t)
		f.store.EXPECT().SystemByKeys(gomock.Any(), "FMS", "CAR").Return(sys, nil)
		var txErr error
		f.store.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, fn func(specdata.Store) error) error {
			txErr = fn(f.store)
			return txErr
		})
		f.store.EXPECT().NextNumber(gomock.Any(), sys.ID).Return(8, nil)
		f.git.EXPECT().BranchHead(gomock.Any(), "tok", "main").Return("base", nil)
		f.git.EXPECT().GetFile(gomock.Any(), "tok", "main", "rules/product/template.md").
			Return(&git.File{Content: []byte("# Product spec: <feature title>\n\n## Problem\n")}, nil)
		f.git.EXPECT().CreateBranch(gomock.Any(), "tok", "feature/FMS.CAR-0008", "base").Return(nil)
		commit := f.git.EXPECT().Commit(gomock.Any(), "tok", "feature/FMS.CAR-0008", gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _, msg string, ch []git.FileChange) (string, error) {
				if ch[0].Path != "specs/FMS/CAR/FMS.CAR-0008/product/spec.md" || string(ch[0].Content) != "# Product spec: Weekend booking\n\n## Problem\n" {
					t.Fatalf("unexpected change %s %q", ch[0].Path, ch[0].Content)
				}
				if tr := git.ParseTrailers(msg); tr.Feature != "FMS.CAR-0008" || tr.Area != "product" {
					t.Fatalf("trailers %+v", tr)
				}
				return "c1", nil
			})
		if commitFails {
			commit.Return("", errors.New("rate limited"))
			f.git.EXPECT().DeleteBranch(gomock.Any(), "tok", "feature/FMS.CAR-0008").Return(nil)
		} else {
			f.git.EXPECT().CreatePR(gomock.Any(), "tok", "feature/FMS.CAR-0008", "main", gomock.Any(), gomock.Any()).Return(&git.PR{Number: 7, URL: "u"}, nil)
			f.store.EXPECT().InsertFeature(gomock.Any(), gomock.Any()).Return(nil)
			f.store.EXPECT().InsertGate(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, g *specdata.Gate) error {
				if g.Area != domain.AreaProduct || g.Status != domain.GateDraft || g.HeadCommit != "c1" {
					t.Fatalf("gate %+v", g)
				}
				return nil
			})
			f.store.EXPECT().InsertEvent(gomock.Any(), gomock.Any()).Return(nil)
		}
		feat, err := f.svc.Create(context.Background(), tu.User("editor:product"), CreateInput{Domain: "FMS", System: "CAR", Title: "Weekend booking"})
		if commitFails {
			if err == nil || txErr == nil {
				t.Fatal("expected failure with rolled back transaction")
			}
			continue
		}
		if err != nil || feat.UniqueID != "FMS.CAR-0008" {
			t.Fatalf("got %v %v", feat, err)
		}
	}
}

func (f *fixture) withFeature(feat *specdata.Feature, gates ...*specdata.Gate) {
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		all = append(all, *g)
	}
	f.store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
}

// DEL-08
func TestDeleteConfirmMismatch(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	err := f.svc.Delete(context.Background(), tu.User("editor:product"), feat.UniqueID, "FMS.CAR-0006")
	tu.Code(t, err, 422, "confirm_mismatch")
}

// DEL-09: editor role needed in every area; a global admin may always delete.
func TestDeleteRequiresEditorOfAllAreas(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft), tu.Gate(feat, domain.AreaDesign, domain.GateDraft))
	err := f.svc.Delete(context.Background(), tu.User("editor:product"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 403, "forbidden")

	f.store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(nil, nil)
	f.git.EXPECT().ClosePR(gomock.Any(), "tok", feat.PRNumber).Return(nil)
	f.git.EXPECT().DeleteBranch(gomock.Any(), "tok", feat.Branch).Return(errors.New("provider down"))
	tu.PassThroughTx(f.store)
	f.store.EXPECT().MarkDeleted(gomock.Any(), feat.ID, gomock.Any(), true).Return(nil) // DEL-15: cleaner retries
	f.store.EXPECT().DropLock(gomock.Any(), feat.ID).Return(nil)
	if err := f.svc.Delete(context.Background(), tu.User("global"), feat.UniqueID, feat.UniqueID); err != nil {
		t.Fatal(err)
	}
}

// DEL-10
func TestDeleteHandedOff(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	feat.Status = domain.FeatureHandedOff
	f.withFeature(feat)
	err := f.svc.Delete(context.Background(), tu.User("global"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 409, "feature_handed_off")
}

// DEL-11
func TestDeleteLockedByOther(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(&specdata.Lock{LockedBy: uuid.New(), LockedByName: "Anna"}, nil)
	err := f.svc.Delete(context.Background(), tu.User("editor:product"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 423, "feature_locked")
}

// DEL-12: a deleted feature answers 410 with author and time.
func TestLoadDeleted(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	feat.Status = domain.FeatureDeleted
	name := "Eugene"
	feat.DeletedByName = &name
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil)
	_, err := Load(context.Background(), f.store, feat.UniqueID)
	tu.Code(t, err, 410, "feature_deleted")
}

// LOCK-02
func TestLockTakenByOther(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat)
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{LockedByName: "Anna"}, false, nil)
	_, err := f.svc.Lock(context.Background(), tu.User("editor:product"), feat.UniqueID)
	tu.Code(t, err, 409, "feature_locked")
}

// NOAPR-04 / HAND-01: permissions reflect approval settings.
func TestPermissions(t *testing.T) {
	feat := tu.Feature(false)
	gates := []specdata.Gate{*tu.Gate(feat, domain.AreaProduct, domain.GateDraft)}
	p := permissions(tu.User("editor:product", "approver:product"), feat, gates)
	if len(p.Submit) != 0 || len(p.Approve) != 0 || !p.Handoff {
		t.Fatalf("no-approval domain: %+v", p)
	}
	feat.ApprovalRequired = true
	p = permissions(tu.User("editor:product"), feat, gates)
	if len(p.Submit) != 1 || p.Handoff || len(p.DeleteGate) != 0 || !p.Delete {
		t.Fatalf("approval domain: %+v", p)
	}
}
