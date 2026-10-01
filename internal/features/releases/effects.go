package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Provider effects of the release and rollback workflows.
const (
	EffectMerge     = "provider.merge"
	EffectMergeSpec = "provider.merge_spec"
	EffectCheckTag  = "provider.check_tag"
	EffectClosePRs  = "provider.close_prs"
)

// Effects executes provider calls (worker). Merges use the token of the
// expert who started the step (arch §8): the action of a person in the
// provider's audit, and branch protection applies.
type Effects struct {
	Q      postgres.Querier
	Store  specdata.Store
	Git    git.Provider
	Tokens git.TokenSource
}

// permanent reports a provider refusal that retries will not fix.
func permanent(err error) bool {
	var ae *git.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
	}
	return errors.Is(err, git.ErrNotFound) || errors.Is(err, git.ErrUnauthorized)
}

// MergePayload is the payload of provider.merge.
type MergePayload struct {
	PRID   uuid.UUID `json:"prId"`
	UserID uuid.UUID `json:"userId"`
}

// Merge merges a service or revert PR after checking it merges cleanly and CI
// is not red; otherwise the step asks for an update_pr task (R27).
func (e *Effects) Merge(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in MergePayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	cd := cycledata.New(e.Q)
	pr, err := cd.PRByID(ctx, in.PRID)
	if err != nil {
		return nil, err
	}
	token, err := e.Tokens.Token(ctx, in.UserID)
	if err != nil {
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": "the expert's git token is not available: " + err.Error()}}}, nil
	}
	p := e.Git.ForRepo(pr.Repo)
	info, err := p.GetPR(ctx, token, pr.Number)
	if err != nil {
		if permanent(err) {
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	switch info.State {
	case "merged":
		return []workflows.NewEvent{{Type: "merged", Payload: map[string]string{"prId": pr.ID.String(), "sha": info.MergeSHA}}}, nil
	case "closed":
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": fmt.Sprintf("PR #%d is closed", pr.Number)}}}, nil
	}
	if info.Mergeable != nil && !*info.Mergeable {
		return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "conflict with the default branch"}}}, nil
	}
	if pr.CIStatus != nil && *pr.CIStatus == "failure" && pr.Kind != "revert" {
		return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "CI is red on the current head"}}}, nil
	}
	if err := p.MergePR(ctx, token, pr.Number, fmt.Sprintf("Merge #%d by Hammurapi release", pr.Number)); err != nil {
		if errors.Is(err, git.ErrConflict) {
			return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "conflict with the default branch"}}}, nil
		}
		if permanent(err) {
			// REL-05: branch protection, missing rights — blocked with the provider's reason.
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	after, err := p.GetPR(ctx, token, pr.Number)
	if err != nil {
		return nil, err
	}
	if err := cd.MarkPRMerged(ctx, pr.ID, after.MergeSHA, &in.UserID); err != nil {
		return nil, err
	}
	return []workflows.NewEvent{{Type: "merged", Payload: map[string]string{"prId": pr.ID.String(), "sha": after.MergeSHA}}}, nil
}

// MergeSpec merges the specification PR on confirmation (R31).
func (e *Effects) MergeSpec(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		FeatureID uuid.UUID `json:"featureId"`
		UserID    uuid.UUID `json:"userId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	f, err := e.Store.FeatureByID(ctx, in.FeatureID)
	if err != nil {
		return nil, err
	}
	token, err := e.Tokens.Token(ctx, in.UserID)
	if err != nil {
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": "the expert's git token is not available: " + err.Error()}}}, nil
	}
	info, err := e.Git.GetPR(ctx, token, f.PRNumber)
	if err == nil && info.State == "merged" {
		return []workflows.NewEvent{{Type: "spec_merged", Payload: map[string]string{"sha": info.MergeSHA}}}, nil
	}
	if err := e.Git.MergePR(ctx, token, f.PRNumber, fmt.Sprintf("%s: specification of the confirmed release", f.UniqueID)); err != nil {
		if permanent(err) || errors.Is(err, git.ErrConflict) {
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	return []workflows.NewEvent{{Type: "spec_merged", Payload: map[string]string{}}}, nil
}

// CheckTag counts a tag containing the merge commit as the release of the
// service (R28): the deploy run gets signal "tag" unless another signal came first.
func (e *Effects) CheckTag(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		DeployRunID uuid.UUID `json:"deployRunId"`
		Repo        string    `json:"repo"`
		MergeSHA    string    `json:"mergeSha"`
		TagSHA      string    `json:"tagSha"`
		Tag         string    `json:"tag"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	p := e.Git.ForRepo(in.Repo)
	token, err := p.BotToken(ctx)
	if err != nil {
		return nil, err
	}
	ok := in.MergeSHA == in.TagSHA
	if !ok {
		if ok, err = p.IsAncestor(ctx, token, in.MergeSHA, in.TagSHA); err != nil {
			if permanent(err) {
				return nil, nil
			}
			return nil, err
		}
	}
	if !ok {
		return nil, nil
	}
	cd := cycledata.New(e.Q)
	tag := in.Tag
	changed, err := cd.UpdateDeployRun(ctx, in.DeployRunID, "success", "tag", &tag, nil, nil, nil)
	if err != nil || !changed {
		return nil, err
	}
	run, err := cd.DeployRunByID(ctx, in.DeployRunID)
	if err != nil {
		return nil, err
	}
	return nil, deploy.Deliver(ctx, e.Q, run)
}

// ClosePRs closes the specification PR without merging and the service PRs
// that were not merged (R32, RB-05, RB-07).
func (e *Effects) ClosePRs(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		FeatureID uuid.UUID `json:"featureId"`
		UserID    uuid.UUID `json:"userId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	f, err := e.Store.FeatureByID(ctx, in.FeatureID)
	if err != nil {
		return nil, err
	}
	if token, err := e.Tokens.Token(ctx, in.UserID); err == nil {
		if err := e.Git.ClosePR(ctx, token, f.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) && !permanent(err) {
			return nil, err
		}
	} else if bot, err := e.Git.BotToken(ctx); err == nil {
		_ = e.Git.ClosePR(ctx, bot, f.PRNumber)
	}
	cd := cycledata.New(e.Q)
	prs, err := cd.FeaturePRs(ctx, f.ID, "service")
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.State != "open" {
			continue
		}
		p := e.Git.ForRepo(pr.Repo)
		if bot, err := p.BotToken(ctx); err == nil {
			if err := p.ClosePR(ctx, bot, pr.Number); err != nil && !errors.Is(err, git.ErrNotFound) && !permanent(err) {
				return nil, err
			}
		}
		if err := cd.MarkPRClosed(ctx, pr.ID); err != nil {
			return nil, err
		}
	}
	return []workflows.NewEvent{{Type: "closed", Payload: map[string]any{}}}, nil
}
