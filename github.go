package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/actions/scaleset"
)

var version = "dev"

func newGitHubClient(t *TargetConfig) (*scaleset.Client, error) {
	info := scaleset.SystemInfo{System: "narc", Version: version, Subsystem: "narc"}
	if t.Auth.TokenFile != "" {
		token, err := readSecretFile(t.Auth.TokenFile)
		if err != nil {
			return nil, err
		}
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: t.URL, PersonalAccessToken: token, SystemInfo: info,
		})
	}
	key, err := readSecretFile(t.Auth.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: t.URL,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: t.Auth.AppClientID, InstallationID: t.Auth.InstallationID, PrivateKey: key},
		SystemInfo:      info,
	})
}

// ScaleSetAPI is the subset of *scaleset.Client used for reconciliation.
type ScaleSetAPI interface {
	GetRunnerGroupByName(ctx context.Context, name string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
}

// reconcileScaleSets makes GitHub match one target's config: it creates or
// updates every configured scale set and deletes ones narc created earlier
// that are no longer configured. It returns the configured scale set IDs by
// name, and the new owned list.
func reconcileScaleSets(ctx context.Context, gh ScaleSetAPI, t *TargetConfig, owned []int, log *slog.Logger) (map[string]int, []int, error) {
	ids := map[string]int{}
	for _, sc := range t.ScaleSets {
		groupID := 1 // the default runner group always has ID 1
		if sc.RunnerGroup != scaleset.DefaultRunnerGroup {
			g, err := gh.GetRunnerGroupByName(ctx, sc.RunnerGroup)
			if err != nil {
				return nil, nil, fmt.Errorf("scale set %s: runner group %s: %w", sc.Name, sc.RunnerGroup, err)
			}
			groupID = g.ID
		}
		want := &scaleset.RunnerScaleSet{
			Name:          sc.Name,
			RunnerGroupID: groupID,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}, // narc installs the runner fresh each boot
		}
		for _, l := range sc.Labels {
			want.Labels = append(want.Labels, scaleset.Label{Name: l, Type: "System"})
		}

		have, err := gh.GetRunnerScaleSet(ctx, groupID, sc.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("scale set %s: get: %w", sc.Name, err)
		}
		switch {
		case have == nil:
			have, err = gh.CreateRunnerScaleSet(ctx, want)
			if err != nil {
				return nil, nil, fmt.Errorf("scale set %s: create: %w", sc.Name, err)
			}
			owned = append(owned, have.ID)
			log.Info("created scale set", "target", t.URL, "scaleset", sc.Name, "id", have.ID)
		case !sameLabels(have.Labels, want.Labels) || have.RunnerSetting != want.RunnerSetting:
			if _, err = gh.UpdateRunnerScaleSet(ctx, have.ID, want); err != nil {
				return nil, nil, fmt.Errorf("scale set %s: update: %w", sc.Name, err)
			}
			log.Info("updated scale set", "target", t.URL, "scaleset", sc.Name, "id", have.ID)
		}
		ids[sc.Name] = have.ID
	}

	configured := map[int]bool{}
	for _, id := range ids {
		configured[id] = true
	}
	var keep []int
	for _, id := range owned {
		if configured[id] {
			if !slices.Contains(keep, id) {
				keep = append(keep, id)
			}
			continue
		}
		err := gh.DeleteRunnerScaleSet(ctx, id)
		switch {
		case err == nil, isNotFound(err):
			log.Info("deleted unconfigured scale set", "target", t.URL, "id", id)
		default:
			// Usually runners still attached; retried on the next startup.
			log.Error("delete unconfigured scale set failed", "target", t.URL, "id", id, "error", err)
			keep = append(keep, id)
		}
	}
	return ids, keep, nil
}

func sameLabels(a, b []scaleset.Label) bool {
	names := func(ls []scaleset.Label) []string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Name)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}
