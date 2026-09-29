package webhooks

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GeenOnGrey/hammurapi-core/internal/domain"
	emocks "github.com/GeenOnGrey/hammurapi-core/internal/platform/events/mocks"
	"github.com/GeenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GeenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GeenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GeenOnGrey/hammurapi-core/internal/testutil"
)

type fixture struct {
	store *smocks.MockStore
	proc  *Processor
	feat  *specdata.Feature
	gates map[domain.Area]*specdata.Gate
	saved []specdata.Gate
	evs   []specdata.GateEvent
}

func setup(t *testing.T, gates ...*specdata.Gate) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), gates: map[domain.Area]*specdata.Gate{}}
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	f.proc = NewProcessor(f.store, nil, ev, nil, nil, "")
	tu.PassThroughTx(f.store)
	f.feat = tu.Feature(true)
	for _, g := range gates {
		g.FeatureID = f.feat.ID
		f.gates[g.Area] = g
	}
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), f.feat.UniqueID).Return(f.feat, nil).AnyTimes()
	f.store.EXPECT().UserIDByUsername(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	f.store.EXPECT().ActiveGate(gomock.Any(), f.feat.ID, gomock.Any()).AnyTimes().
		DoAndReturn(func(_ context.Context, _ uuid.UUID, a domain.Area) (*specdata.Gate, error) {
			if g, ok := f.gates[a]; ok && g.DeletedAt == nil {
				return g, nil
			}
			return nil, specdata.ErrNotFound
		})
	f.store.EXPECT().ActiveGates(gomock.Any(), f.feat.ID).AnyTimes().DoAndReturn(func(context.Context, uuid.UUID) ([]specdata.Gate, error) {
		var out []specdata.Gate
		for _, g := range f.gates {
			if g.DeletedAt == nil {
				out = append(out, *g)
			}
		}
		return out, nil
	})
	f.store.EXPECT().SaveGate(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(func(_ context.Context, g *specdata.Gate) error {
		f.saved = append(f.saved, *g)
		return nil
	})
	f.store.EXPECT().InsertEvent(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(func(_ context.Context, e *specdata.GateEvent) error {
		f.evs = append(f.evs, *e)
		return nil
	})
	return f
}

func (f *fixture) push(id string, commits ...git.PushCommit) error {
	f.store.EXPECT().MarkWebhookProcessed(gomock.Any(), id).Return(true, nil)
	return f.proc.Apply(context.Background(), &git.PushEvent{EventID: id, Branch: "feature/" + f.feat.UniqueID, Actor: "anna", Commits: commits})
}

func (f *fixture) types() []domain.GateEventType {
	var out []domain.GateEventType
	for _, e := range f.evs {
		out = append(out, e.Type)
	}
	return out
}

const dir = "specs/FMS/CAR/FTR.FMS.CAR-0005/"

// GATE-02 / GATE-04: an edit made anywhere resets an in-review gate to draft.
func TestEditResetsGate(t *testing.T) {
	f := setup(t, &specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateInReview, HeadCommit: "c0"})
	if err := f.push("e1", git.PushCommit{SHA: "c1", Message: "direct edit", Modified: []string{dir + "product/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	g := f.gates[domain.AreaProduct]
	if g.Status != domain.GateDraft || g.HeadCommit != "c1" || g.SubmittedAt != nil {
		t.Fatalf("gate %+v", g)
	}
	if got := f.types(); len(got) != 2 || got[0] != domain.EventEdited || got[1] != domain.EventReset {
		t.Fatalf("events %v", got)
	}
}

// HOOK-02: a redelivered event is processed once.
func TestDuplicateDelivery(t *testing.T) {
	f := setup(t, &specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "c0"})
	f.store.EXPECT().MarkWebhookProcessed(gomock.Any(), "dup").Return(false, nil)
	err := f.proc.Apply(context.Background(), &git.PushEvent{EventID: "dup", Branch: "feature/" + f.feat.UniqueID,
		Commits: []git.PushCommit{{SHA: "c1", Modified: []string{dir + "product/spec.md"}}}})
	if err != nil || len(f.evs) != 0 || f.gates[domain.AreaProduct].Status != domain.GateApproved {
		t.Fatalf("processed twice: %v %v", err, f.evs)
	}
}

// HOOK-03 / HOOK-04: a push touching two areas edits both; the agent trailer is kept.
func TestTwoAreasAndAgentTrailer(t *testing.T) {
	f := setup(t,
		&specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "a"},
		&specdata.Gate{ID: uuid.New(), Area: domain.AreaDesign, Status: domain.GateInReview, HeadCommit: "b"})
	msg := git.Trailers{Feature: "FMS.CAR-0005", Agent: true}.Message("agent edit")
	if err := f.push("e2", git.PushCommit{SHA: "c", Message: msg, Modified: []string{dir + "product/spec.md", dir + "design/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []domain.Area{domain.AreaProduct, domain.AreaDesign} {
		if f.gates[a].Status != domain.GateDraft {
			t.Fatalf("%s not reset", a)
		}
	}
	for _, e := range f.evs {
		if !e.IsAgent {
			t.Fatalf("event %s without agent flag", e.Type)
		}
	}
}

// IMP-03: the import's own commit (already the gate head) does not reset the gate.
func TestKnownCommitSkipped(t *testing.T) {
	f := setup(t, &specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateInReview, HeadCommit: "imp"})
	if err := f.push("e3", git.PushCommit{SHA: "imp", Added: []string{dir + "product/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	if f.gates[domain.AreaProduct].Status != domain.GateInReview || len(f.evs) != 0 {
		t.Fatalf("known commit was projected")
	}
}

// HOOK-05 / HOOK-06: rules/ and foreign paths leave the projection untouched.
func TestIgnoredPaths(t *testing.T) {
	f := setup(t, &specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "a"})
	if err := f.push("e4", git.PushCommit{SHA: "x", Modified: []string{"rules/product/template.md", "README.md", "specs/FMS/CAR/FMS.CAR-0009/product/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.evs) != 0 {
		t.Fatalf("events %v", f.types())
	}
	// Pushes to non-feature branches are ignored without touching the store.
	if err := f.proc.Apply(context.Background(), &git.PushEvent{EventID: "e5", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
}

// DEL-14: removing spec.md directly in git deletes the gate, unless it is the last one.
func TestSpecRemovedInGit(t *testing.T) {
	f := setup(t,
		&specdata.Gate{ID: uuid.New(), Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "a"},
		&specdata.Gate{ID: uuid.New(), Area: domain.AreaDesign, Status: domain.GateDraft, HeadCommit: "b"})
	if err := f.push("e6", git.PushCommit{SHA: "rm", Removed: []string{dir + "design/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	if f.gates[domain.AreaDesign].DeletedAt == nil || f.types()[0] != domain.EventDeleted {
		t.Fatalf("design not deleted: %v", f.types())
	}
	// The remaining last gate is kept.
	if err := f.push("e7", git.PushCommit{SHA: "rm2", Removed: []string{dir + "product/spec.md"}}); err != nil {
		t.Fatal(err)
	}
	if f.gates[domain.AreaProduct].DeletedAt != nil {
		t.Fatal("last gate deleted")
	}
}
