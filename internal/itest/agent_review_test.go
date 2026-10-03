//go:build integration

package itest

import (
	"bytes"
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentcfg"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
)

// SK-05: a change of the scenario bindings alone (agent/skills.yaml) keeps
// the archive hash, but the snapshot's bindings follow it.
func TestSkillBindingsFollowSkillsYAML(t *testing.T) {
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM agent_skill_snapshots`)
	must(t, err)
	ctrl := gomock.NewController(t)
	prov := mocks.NewMockProvider(ctrl)
	yaml := "skills:\n  spec-review:\n    scenarios: [chat]\n"
	prov.EXPECT().BranchHead(gomock.Any(), gomock.Any(), "main").Return("c1", nil).AnyTimes()
	prov.EXPECT().ListFiles(gomock.Any(), gomock.Any(), gomock.Any(), agentcfg.SkillsDir).
		Return([]string{agentcfg.SkillsDir + "/spec-review/SKILL.md"}, nil).AnyTimes()
	prov.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), agentcfg.SkillsDir+"/spec-review/SKILL.md").
		Return(&git.File{Content: []byte("---\nname: spec-review\ndescription: Reviews specs\n---\nBody\n")}, nil).AnyTimes()
	prov.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), agentcfg.SkillsYAML).
		DoAndReturn(func(context.Context, string, string, string) (*git.File, error) { return &git.File{Content: []byte(yaml)}, nil }).AnyTimes()
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	must(t, err)
	s := agentcfg.NewService(pool, box, fakeOperator{}, nil, &memStorage{}, prov, nil, "main")

	h1, err := s.SyncSnapshot(ctx, "bot")
	must(t, err)
	yaml = "skills:\n  spec-review:\n    scenarios: [codegen]\n"
	h2, err := s.SyncSnapshot(ctx, "bot")
	must(t, err)
	if h1 != h2 {
		t.Fatalf("the archive hash changed: %s %s", h1, h2)
	}
	var raw string
	must(t, pool.QueryRow(ctx, `SELECT skills::text FROM agent_skill_snapshots WHERE hash = $1`, h2).Scan(&raw))
	if !bytes.Contains([]byte(raw), []byte(`"codegen"`)) || bytes.Contains([]byte(raw), []byte(`"chat"`)) {
		t.Fatalf("bindings not refreshed: %s", raw)
	}
}

// A scenario without a model does not hide later scenarios with overrides.
func TestResolvedModelsSkipsUnresolved(t *testing.T) {
	ctx := context.Background()
	s, admin := agentSvc(t)
	c, err := s.CreateConnection(ctx, admin, agentcfg.ConnectionInput{Name: "Code", Type: agentcfg.TypeDeepSeek, APIKey: "sk-cccc-33333333"})
	must(t, err)
	// Only an override, no default model (possible after the default's connection is gone).
	_, err = pool.Exec(ctx, `INSERT INTO agent_scenario_models (scenario, connection_id, model, thinking) VALUES ('codegen', $1, 'deepseek-v4-pro', 'off')`, c.ID)
	must(t, err)
	got, err := s.ResolvedModels(ctx, admin)
	must(t, err)
	if len(got) != 1 || got[0].Scenario != agent.ScenarioCodegen || got[0].ConnectionID != c.ID {
		t.Fatalf("%+v", got)
	}
}
