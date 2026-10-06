package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/hashicorp/nomad/api"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeGitHub struct {
	nextID  int
	removed []int64
	byName  map[string]int
}

func (f *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.nextID++
	if f.byName == nil {
		f.byName = map[string]int{}
	}
	f.byName[s.Name] = f.nextID
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.nextID, Name: s.Name}, EncodedJITConfig: "jit-" + s.Name}, nil
}

func (f *fakeGitHub) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	if id, ok := f.byName[name]; ok {
		return &scaleset.RunnerReference{ID: id, Name: name}, nil
	}
	return nil, nil
}

func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}

type fakeNomad struct {
	vars         map[string]map[string]string
	jobs         map[string]*api.JobListStub // dispatched children by ID
	allocs       map[string][]*api.AllocationListStub
	stopped      []string
	failDispatch bool
	n            int
}

func newFakeNomad() *fakeNomad {
	return &fakeNomad{vars: map[string]map[string]string{}, jobs: map[string]*api.JobListStub{}, allocs: map[string][]*api.AllocationListStub{}}
}

func (f *fakeNomad) PutVar(_ context.Context, p string, items map[string]string) error {
	f.vars[p] = items
	return nil
}
func (f *fakeNomad) DeleteVar(_ context.Context, p string) error { delete(f.vars, p); return nil }
func (f *fakeNomad) ListVars(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for p := range f.vars {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeNomad) Dispatch(_ context.Context, job, name string) (string, error) {
	if f.failDispatch {
		return "", errors.New("boom")
	}
	f.n++
	id := fmt.Sprintf("%s/dispatch-%d", job, f.n)
	f.jobs[id] = &api.JobListStub{ID: id, ParentID: job, Status: "pending", Meta: map[string]string{"runner_name": name}, SubmitTime: time.Now().UnixNano()}
	return id, nil
}
func (f *fakeNomad) Children(_ context.Context, job string) ([]*api.JobListStub, error) {
	var out []*api.JobListStub
	for _, j := range f.jobs {
		if j.ParentID == job {
			out = append(out, j)
		}
	}
	return out, nil
}
func (f *fakeNomad) Allocs(_ context.Context, id string) ([]*api.AllocationListStub, error) {
	return f.allocs[id], nil
}
func (f *fakeNomad) Stop(_ context.Context, id string) error {
	f.stopped = append(f.stopped, id)
	return nil
}

func newTestScaler(gh GitHub, n Nomad, max, warm int) *Scaler {
	cfg := &ScaleSetConfig{Name: "nomad-linux", Job: "runner", Max: max, Warm: warm, MaxDuration: duration{time.Hour}}
	return NewScaler(cfg, "https://github.com/o", 7, gh, n, discard)
}

func TestDesired(t *testing.T) {
	for _, c := range []struct{ demand, warm, max, want int }{
		{0, 0, 4, 0}, {0, 1, 4, 1}, {3, 1, 4, 4}, {10, 1, 4, 4}, {2, 0, 4, 2}, {0, 4, 4, 4},
	} {
		if got := desired(c.demand, c.warm, c.max); got != c.want {
			t.Errorf("desired(%d, %d, %d) = %d, want %d", c.demand, c.warm, c.max, got, c.want)
		}
	}
}

func TestRunnerName(t *testing.T) {
	gh, n := &fakeGitHub{}, newFakeNomad()
	s := newTestScaler(gh, n, 1, 1)
	s.HandleDesiredRunnerCount(t.Context(), 0)
	for name := range s.runners {
		if id, ok := parseRunnerName(name); !ok || id != 7 {
			t.Errorf("parseRunnerName(%q) = %d, %v", name, id, ok)
		}
	}
	if _, ok := parseRunnerName("group"); ok {
		t.Error("non-runner name parsed")
	}
}

func TestLifecycle(t *testing.T) {
	ctx := t.Context()
	gh, n := &fakeGitHub{}, newFakeNomad()
	s := newTestScaler(gh, n, 3, 1)

	// Demand 5, max 3: three dispatches, each with its variable written first.
	if got, _ := s.HandleDesiredRunnerCount(ctx, 5); got != 3 {
		t.Fatalf("runners = %d, want 3", got)
	}
	if len(n.jobs) != 3 || len(n.vars) != 3 {
		t.Fatalf("jobs=%d vars=%d, want 3/3", len(n.jobs), len(n.vars))
	}
	for p, v := range n.vars {
		if !strings.HasPrefix(p, "nomad/jobs/runner/nomad-linux-7-") || !strings.HasPrefix(v["jitconfig"], "jit-") {
			t.Errorf("bad variable %s = %v", p, v)
		}
	}

	// Demand drops: no scale down.
	if got, _ := s.HandleDesiredRunnerCount(ctx, 0); got != 3 {
		t.Fatalf("scaled down to %d", got)
	}

	ids := slices.Sorted(maps.Keys(n.jobs))
	r0 := s.byJob(ids[0])

	// Prestart task started, main task pending: still starting.
	tasks := map[string]*api.TaskState{"fetch": {StartedAt: time.Now()}, "vm": {}}
	s.Observe(ctx, ids[0], "pending", tasks)
	if r0.started {
		t.Fatal("started before every task started")
	}
	// Every task started: booted, but the variable stays until the allocation
	// ends, so a restarted Nomad agent can render it again.
	tasks["vm"] = &api.TaskState{StartedAt: time.Now()}
	s.Observe(ctx, ids[0], "running", tasks)
	if !r0.started {
		t.Fatal("not started once every task started")
	}
	if _, ok := n.vars[varPath("runner", r0.name)]; !ok {
		t.Fatal("variable deleted while the allocation runs")
	}

	// Runner 0 runs a job and completes: no deregistration.
	s.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: r0.name, RunnerID: 1})
	s.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: r0.name, RunnerID: 1})
	s.demand = 0
	s.Observe(ctx, ids[0], "complete", tasks)
	if len(gh.removed) != 0 {
		t.Fatalf("completed runner deregistered: %v", gh.removed)
	}
	if _, ok := n.vars[varPath("runner", r0.name)]; ok {
		t.Fatal("variable not deleted when the allocation ended")
	}
	// Demand 0 + warm 1 = 1 < 2 live, so no replacement.
	if len(s.runners) != 2 {
		t.Fatalf("runners = %d, want 2", len(s.runners))
	}

	// Runner 1's VM dies before taking a job: deregistered and variable deleted.
	r1 := s.byJob(ids[1])
	s.Observe(ctx, ids[1], "failed", nil)
	if !slices.Contains(gh.removed, r1.runnerID) {
		t.Fatalf("dead runner not deregistered: %v", gh.removed)
	}
	if _, ok := n.vars[varPath("runner", r1.name)]; ok {
		t.Fatal("variable not deleted on terminal allocation")
	}

	// A freed slot is refilled from the last demand without waiting for GitHub.
	s.demand = 5
	s.Observe(ctx, ids[2], "failed", nil)
	if len(s.runners) != 3 {
		t.Fatalf("runners = %d after refill, want 3", len(s.runners))
	}

	// Allocation events for unknown jobs aren't ours.
	if s.Observe(ctx, "other/dispatch-1", "complete", nil) {
		t.Error("claimed a foreign allocation")
	}
}

func TestDispatchFailureCleansUp(t *testing.T) {
	gh, n := &fakeGitHub{}, newFakeNomad()
	n.failDispatch = true
	s := newTestScaler(gh, n, 2, 0)
	s.HandleDesiredRunnerCount(t.Context(), 1)
	if len(s.runners) != 0 || len(n.vars) != 0 || len(gh.removed) != 1 {
		t.Fatalf("runners=%d vars=%d removed=%v", len(s.runners), len(n.vars), gh.removed)
	}
}

func TestReconcile(t *testing.T) {
	ctx := t.Context()
	gh, n := &fakeGitHub{}, newFakeNomad()
	s := newTestScaler(gh, n, 3, 0)
	s.HandleDesiredRunnerCount(ctx, 3)
	ids := slices.Sorted(maps.Keys(n.jobs))

	n.jobs[ids[0]].Status = "dead" // event missed
	delete(n.jobs, ids[1])         // purged by someone
	n.allocs[ids[2]] = []*api.AllocationListStub{{ClientStatus: "running", TaskStates: map[string]*api.TaskState{"vm": {StartedAt: time.Now()}}}}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // over max duration
	s.demand = 0

	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.runners) != 1 || len(gh.removed) != 2 {
		t.Fatalf("runners=%d removed=%v", len(s.runners), gh.removed)
	}
	if _, ok := n.vars[varPath("runner", s.byJob(ids[2]).name)]; len(n.vars) != 1 || !ok {
		t.Fatalf("variables left: %v", n.vars)
	}
	if !slices.Equal(n.stopped, []string{ids[2]}) {
		t.Fatalf("stopped = %v", n.stopped)
	}
}

func TestReconcileStopsAllocOutlivingJob(t *testing.T) {
	ctx := t.Context()
	gh, n := &fakeGitHub{}, newFakeNomad()
	s := newTestScaler(gh, n, 1, 0)
	s.HandleDesiredRunnerCount(ctx, 1)
	var r *runner
	for _, v := range s.runners {
		r = v
	}
	n.jobs[r.jobID].Status = "running"
	s.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: r.name})
	s.demand = 0

	s.Reconcile(ctx)
	if len(n.stopped) != 0 {
		t.Fatalf("stopped within grace: %v", n.stopped)
	}
	s.now = func() time.Time { return time.Now().Add(completedGrace + time.Minute) }
	s.Reconcile(ctx)
	if !slices.Equal(n.stopped, []string{r.jobID}) {
		t.Fatalf("stopped = %v", n.stopped)
	}
}

func TestRecover(t *testing.T) {
	ctx := t.Context()
	gh, n := &fakeGitHub{}, newFakeNomad()
	a := newTestScaler(gh, n, 3, 0)
	b := newTestScaler(gh, n, 3, 0)
	b.ID, b.Name = 8, "other"

	n.jobs["runner/dispatch-a"] = &api.JobListStub{ID: "runner/dispatch-a", ParentID: "runner", Status: "running", Meta: map[string]string{"runner_name": "nomad-linux-7-0000000a"}}
	n.jobs["runner/dispatch-b"] = &api.JobListStub{ID: "runner/dispatch-b", ParentID: "runner", Status: "pending", Meta: map[string]string{"runner_name": "other-8-0000000b"}}
	n.jobs["runner/dispatch-c"] = &api.JobListStub{ID: "runner/dispatch-c", ParentID: "runner", Status: "dead", Meta: map[string]string{"runner_name": "nomad-linux-7-0000000c"}}
	for _, p := range []string{
		"nomad/jobs/runner/nomad-linux-7-0000000a", // live: keep
		"nomad/jobs/runner/nomad-linux-7-0000000c", // dead child: sweep
		"nomad/jobs/runner/nomad-linux-7-0000000d", // no child: sweep
		"nomad/jobs/runner/group",                  // not ours: keep
		"nomad/jobs/runner-x/nomad-linux-7-0000000e",
	} {
		n.vars[p] = map[string]string{}
	}

	if err := Recover(ctx, n, "runner", []*Scaler{a, b}, discard); err != nil {
		t.Fatal(err)
	}
	if len(a.runners) != 1 || a.runners["nomad-linux-7-0000000a"] == nil || len(b.runners) != 1 {
		t.Fatalf("adopted a=%v b=%v", a.runners, b.runners)
	}
	want := []string{"nomad/jobs/runner-x/nomad-linux-7-0000000e", "nomad/jobs/runner/group", "nomad/jobs/runner/nomad-linux-7-0000000a"}
	if got := slices.Sorted(maps.Keys(n.vars)); !slices.Equal(got, want) {
		t.Fatalf("vars after sweep = %v", got)
	}

	// An adopted runner with no known runner ID is looked up by name on death.
	gh.byName = map[string]int{"nomad-linux-7-0000000a": 42}
	a.Observe(ctx, "runner/dispatch-a", "failed", nil)
	if !slices.Equal(gh.removed, []int64{42}) {
		t.Fatalf("removed = %v", gh.removed)
	}
}

func TestCheckRunnerJob(t *testing.T) {
	zero, f := 0, false
	good := func() *api.Job {
		return &api.Job{
			ID:               new("runner"),
			Type:             new("batch"),
			ParameterizedJob: &api.ParameterizedJobConfig{MetaRequired: []string{"runner_name"}},
			TaskGroups: []*api.TaskGroup{{
				Name:             new("g"),
				RestartPolicy:    &api.RestartPolicy{Attempts: &zero},
				ReschedulePolicy: &api.ReschedulePolicy{Attempts: &zero, Unlimited: &f},
			}},
		}
	}
	if err := checkRunnerJob(good()); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*api.Job){
		"service":       func(j *api.Job) { j.Type = new("service") },
		"not param":     func(j *api.Job) { j.ParameterizedJob = nil },
		"no meta":       func(j *api.Job) { j.ParameterizedJob.MetaRequired = nil },
		"restarts":      func(j *api.Job) { j.TaskGroups[0].RestartPolicy.Attempts = new(2) },
		"reschedules":   func(j *api.Job) { j.TaskGroups[0].ReschedulePolicy.Attempts = new(1) },
		"unlimited":     func(j *api.Job) { j.TaskGroups[0].ReschedulePolicy.Unlimited = new(true) },
		"no reschedule": func(j *api.Job) { j.TaskGroups[0].ReschedulePolicy = nil },
	} {
		j := good()
		mut(j)
		if checkRunnerJob(j) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type fakeScaleSets struct {
	existing  map[string]*scaleset.RunnerScaleSet
	deleted   []int
	updated   []int
	deleteErr error
	next      int
}

func (f *fakeScaleSets) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	return &scaleset.RunnerGroup{ID: 5, Name: name}, nil
}
func (f *fakeScaleSets) GetRunnerScaleSet(_ context.Context, _ int, name string) (*scaleset.RunnerScaleSet, error) {
	return f.existing[name], nil
}
func (f *fakeScaleSets) CreateRunnerScaleSet(_ context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.next++
	rs.ID = 100 + f.next
	return rs, nil
}
func (f *fakeScaleSets) UpdateRunnerScaleSet(_ context.Context, id int, _ *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updated = append(f.updated, id)
	return nil, nil
}
func (f *fakeScaleSets) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func TestReconcileScaleSets(t *testing.T) {
	gh := &fakeScaleSets{existing: map[string]*scaleset.RunnerScaleSet{
		"same":    {ID: 1, Labels: []scaleset.Label{{Name: "same"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}},
		"relabel": {ID: 2, Labels: []scaleset.Label{{Name: "old"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}},
	}}
	tc := &TargetConfig{URL: "https://github.com/o", ScaleSets: []*ScaleSetConfig{
		{Name: "same", Labels: []string{"same"}, RunnerGroup: "default"},
		{Name: "relabel", Labels: []string{"relabel"}, RunnerGroup: "default"},
		{Name: "new", Labels: []string{"new"}, RunnerGroup: "ci"},
	}}
	// 1 is owned and configured; 50 is owned and gone from config; 2 was never ours.
	ids, owned, err := reconcileScaleSets(t.Context(), gh, tc, []int{1, 50}, discard)
	if err != nil {
		t.Fatal(err)
	}
	if ids["same"] != 1 || ids["relabel"] != 2 || ids["new"] != 101 {
		t.Fatalf("ids = %v", ids)
	}
	if !slices.Equal(gh.updated, []int{2}) || !slices.Equal(gh.deleted, []int{50}) {
		t.Fatalf("updated=%v deleted=%v", gh.updated, gh.deleted)
	}
	if slices.Sort(owned); !slices.Equal(owned, []int{1, 101}) {
		t.Fatalf("owned = %v", owned)
	}

	// A failed delete stays owned so the next startup retries it.
	gh = &fakeScaleSets{existing: map[string]*scaleset.RunnerScaleSet{}, deleteErr: errors.New("runners still attached")}
	_, owned, _ = reconcileScaleSets(t.Context(), gh, &TargetConfig{}, []int{50}, discard)
	if !slices.Equal(owned, []int{50}) {
		t.Fatalf("owned = %v", owned)
	}
}
