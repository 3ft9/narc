package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/hashicorp/nomad/api"
)

// GitHub is the subset of *scaleset.Client the scaler uses.
type GitHub interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// Nomad is the subset of the Nomad API the scaler uses. See nomad.go.
type Nomad interface {
	PutVar(ctx context.Context, path string, items map[string]string) error
	DeleteVar(ctx context.Context, path string) error
	ListVars(ctx context.Context, prefix string) ([]string, error)
	Dispatch(ctx context.Context, job string, meta map[string]string) (dispatchedID string, err error)
	Children(ctx context.Context, job string) ([]*api.JobListStub, error)
	Allocs(ctx context.Context, jobID string) ([]*api.AllocationListStub, error)
	Stop(ctx context.Context, jobID string) error
}

type runner struct {
	name       string
	jobID      string // dispatched Nomad job ID
	runnerID   int64  // GitHub runner ID; 0 if unknown (adopted on startup)
	dispatched time.Time
	adopted    bool
	started    bool      // every task has started: booted, waiting for a job
	busy       bool      // JobStarted seen
	completed  time.Time // JobCompleted seen, so GitHub already removed the runner
}

// Scaler manages the runners of one scale set. It implements listener.Scaler.
//
// ponytail: one mutex held across GitHub/Nomad calls; fine at tens of runners
// per scale set, split the lock if dispatch latency starts to matter.
type Scaler struct {
	Target      string
	Name        string
	ID          int
	Job         string
	Max, Warm   int
	MaxDuration time.Duration

	gh    GitHub
	nomad Nomad
	log   *slog.Logger
	now   func() time.Time

	active atomic.Bool // has a live message session

	mu       sync.Mutex
	demand   int
	runners  map[string]*runner
	poststop []string // runner job tasks that only start once the runner has ended

	waitingSince time.Time // since when jobs have been waiting; zero if none are
}

var _ listener.Scaler = (*Scaler)(nil)

func NewScaler(cfg *ScaleSetConfig, target string, id int, gh GitHub, nomad Nomad, log *slog.Logger) *Scaler {
	return &Scaler{
		Target: target, Name: cfg.Name, ID: id, Job: cfg.Job,
		Max: cfg.Max, Warm: cfg.Warm, MaxDuration: cfg.MaxDuration.Duration,
		gh: gh, nomad: nomad, now: time.Now,
		log:     log.With("target", target, "scaleset", cfg.Name),
		runners: map[string]*runner{},
	}
}

// Runner names are "<scaleset>-<scaleset ID>-<8 hex>". The ID lets startup
// recovery map a dispatched child back to its scale set when several scale
// sets share a runner job.
var runnerNameRE = regexp.MustCompile(`^.+-(\d+)-[0-9a-f]{8}$`)

func parseRunnerName(name string) (scaleSetID int, ok bool) {
	m := runnerNameRE.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	return id, err == nil
}

// dispatchMeta is the meta narc dispatches runner jobs with. narc_target and
// narc_scaleset tell startup recovery which scale set owns a runner: scale set
// IDs are only unique per target, and several targets may share a runner job.
var dispatchMeta = []string{"runner_name", "narc_target", "narc_scaleset"}

func varPath(job, runnerName string) string { return "nomad/jobs/" + job + "/" + runnerName }

// desired is the number of runners that should be alive: one per assigned job
// plus the warm pool, capped at max.
func desired(demand, warm, max int) int { return min(max, demand+warm) }

func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demand = count
	if err := s.scaleUp(ctx); err != nil {
		// Don't kill the session: the next poll (at most ~50s away) retries.
		s.log.Error("scale up failed", "error", err)
	}
	return len(s.runners), nil
}

func (s *Scaler) HandleJobStarted(ctx context.Context, j *scaleset.JobStarted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.Info("job started", "runner", j.RunnerName, "repo", j.OwnerName+"/"+j.RepositoryName, "job", j.JobDisplayName)
	if r := s.runners[j.RunnerName]; r != nil {
		r.busy = true
		r.runnerID = int64(j.RunnerID)
		if !r.adopted {
			bootToJobStart.WithLabelValues(s.Target, s.Name).Observe(s.now().Sub(r.dispatched).Seconds())
		}
	}
	s.updateGauge()
	return nil
}

func (s *Scaler) HandleJobCompleted(ctx context.Context, j *scaleset.JobCompleted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.Info("job completed", "runner", j.RunnerName, "result", j.Result)
	if r := s.runners[j.RunnerName]; r != nil {
		r.completed = s.now()
	}
	// The slot frees when the allocation ends (VM powered off), not here,
	// so narc never has more than max VMs at once.
	return nil
}

func (s *Scaler) scaleUp(ctx context.Context) error {
	defer s.updateGauge()
	for len(s.runners) < desired(s.demand, s.Warm, s.Max) {
		if err := s.start(ctx); err != nil {
			dispatchFailures.WithLabelValues(s.Target, s.Name).Inc()
			return err
		}
	}
	return nil
}

func (s *Scaler) start(ctx context.Context) error {
	b := make([]byte, 4)
	rand.Read(b)
	name := fmt.Sprintf("%s-%d-%s", s.Name, s.ID, hex.EncodeToString(b))

	jit, err := s.gh.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, s.ID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	r := &runner{name: name, dispatched: s.now()}
	if jit.Runner != nil {
		r.runnerID = int64(jit.Runner.ID)
	}

	// Write first, always: the variable must exist before the job's template renders.
	path := varPath(s.Job, name)
	if err := s.nomad.PutVar(ctx, path, map[string]string{"jitconfig": jit.EncodedJITConfig}); err != nil {
		s.deregister(ctx, r)
		return fmt.Errorf("write variable %s: %w", path, err)
	}
	r.jobID, err = s.nomad.Dispatch(ctx, s.Job, map[string]string{"runner_name": name, "narc_target": s.Target, "narc_scaleset": s.Name})
	if err != nil {
		s.deleteVar(ctx, r)
		s.deregister(ctx, r)
		return fmt.Errorf("dispatch %s: %w", s.Job, err)
	}
	s.runners[name] = r
	s.log.Info("dispatched runner", "runner", name, "nomad_job", r.jobID)
	return nil
}

// Observe applies an allocation update from the event stream. It returns
// false if the allocation isn't one of this scale set's runners.
func (s *Scaler) Observe(ctx context.Context, jobID, clientStatus string, tasks map[string]*api.TaskState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byJob(jobID)
	if r == nil {
		return false
	}
	if s.observe(ctx, r, clientStatus, tasks) {
		// A slot freed up; refill it from the last known demand.
		if err := s.scaleUp(ctx); err != nil {
			s.log.Error("scale up failed", "error", err)
		}
	}
	s.updateGauge()
	return true
}

// observe returns true if the runner has ended.
func (s *Scaler) observe(ctx context.Context, r *runner, clientStatus string, tasks map[string]*api.TaskState) bool {
	switch clientStatus {
	case api.AllocClientStatusComplete, api.AllocClientStatusFailed, api.AllocClientStatusLost:
		s.finish(ctx, r, "allocation "+clientStatus)
		return true
	}
	if allStarted(tasks, s.poststop) {
		r.started = true
	}
	return false
}

// allStarted reports whether every task except the poststop ones has
// started at least once.
func allStarted(tasks map[string]*api.TaskState, poststop []string) bool {
	if len(tasks) == 0 {
		return false
	}
	for name, t := range tasks {
		if (t == nil || t.StartedAt.IsZero()) && !slices.Contains(poststop, name) {
			return false
		}
	}
	return true
}

// SetPoststop records the runner job's poststop tasks, which allStarted
// mustn't wait for.
func (s *Scaler) SetPoststop(tasks []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.poststop = tasks
}

func (s *Scaler) byJob(jobID string) *runner {
	for _, r := range s.runners {
		if r.jobID == jobID {
			return r
		}
	}
	return nil
}

// finish forgets a runner whose allocation or job has ended, cleaning up its
// variable and, if GitHub never saw it complete a job, its registration.
//
// The variable lives until now, not just until the task starts: a restarted
// Nomad agent re-renders the task's templates, and a nomadVar on a deleted
// variable blocks forever, so the task would never see its VM exit. A
// template that doesn't block instead re-renders the file without the JIT
// config while the VM may still be reading it.
func (s *Scaler) finish(ctx context.Context, r *runner, why string) {
	s.log.Info("runner ended", "runner", r.name, "reason", why, "completed_job", !r.completed.IsZero())
	s.deleteVar(ctx, r)
	if r.completed.IsZero() {
		s.deregister(ctx, r)
	}
	delete(s.runners, r.name)
}

func (s *Scaler) deleteVar(ctx context.Context, r *runner) {
	if err := s.nomad.DeleteVar(ctx, varPath(s.Job, r.name)); err != nil {
		s.log.Error("delete variable failed", "runner", r.name, "error", err)
	}
}

func (s *Scaler) deregister(ctx context.Context, r *runner) {
	id := r.runnerID
	if id == 0 {
		ref, err := s.gh.GetRunnerByName(ctx, r.name)
		if err != nil {
			s.log.Error("look up runner failed", "runner", r.name, "error", err)
			return
		}
		if ref == nil {
			return // never registered, or already gone
		}
		id = int64(ref.ID)
	}
	err := s.gh.RemoveRunner(ctx, id)
	if err != nil && !strings.Contains(err.Error(), scaleset.RunnerNotFoundError.Error()) {
		s.log.Error("deregister runner failed", "runner", r.name, "error", err)
		return
	}
	deregistrations.WithLabelValues(s.Target, s.Name).Inc()
	s.log.Info("deregistered runner", "runner", r.name)
}

// completedGrace is how long a runner's allocation may outlive its GitHub
// job. The VM powers off seconds after the runner exits; an allocation still
// running long after that is stuck (a VM that never powered off, or a task
// runner that lost track of its VM) and would otherwise hold a slot until
// max_duration.
const completedGrace = 10 * time.Minute

// stuckTimeout is how long jobs may wait at GitHub while runners sit idle.
// A healthy idle runner takes a waiting job within seconds, so idle runners
// older than this are taken never to have come online (cloud-init failed,
// say) and are replaced. GitHub's scale set API doesn't say which runners are
// online.
const stuckTimeout = 15 * time.Minute

// jobsStuck reports whether GitHub has had more jobs assigned than this
// scale set has busy runners for longer than stuckTimeout. Adopted runners
// count as busy: their JobStarted, if any, came before narc restarted.
func (s *Scaler) jobsStuck() bool {
	taken := 0
	for _, r := range s.runners {
		if r.busy || r.adopted {
			taken++
		}
	}
	if s.demand <= taken {
		s.waitingSince = time.Time{}
		return false
	}
	if s.waitingSince.IsZero() {
		s.waitingSince = s.now()
	}
	return s.now().Sub(s.waitingSince) > stuckTimeout
}

// Reconcile is the periodic backstop for everything the event stream should
// have told us: ended jobs, started tasks, runners whose allocation outlives
// their job, idle runners that never take a waiting job, and runners over
// max duration.
func (s *Scaler) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	children, err := s.nomad.Children(ctx, s.Job)
	if err != nil {
		return err
	}
	status := map[string]string{}
	for _, c := range children {
		status[c.ID] = c.Status
	}
	stuck := s.jobsStuck()
	for _, r := range s.runners {
		if st, ok := status[r.jobID]; !ok || st == "dead" {
			s.finish(ctx, r, "nomad job gone or dead")
			continue
		}
		if !r.started {
			allocs, err := s.nomad.Allocs(ctx, r.jobID)
			if err != nil {
				s.log.Error("list allocations failed", "runner", r.name, "error", err)
			}
			if len(allocs) > 0 && s.observe(ctx, r, allocs[0].ClientStatus, allocs[0].TaskStates) {
				continue
			}
		}
		var why string
		switch {
		case !r.completed.IsZero() && s.now().Sub(r.completed) > completedGrace:
			why = "allocation still running after its job completed"
		case stuck && !r.busy && !r.adopted && s.now().Sub(r.dispatched) > stuckTimeout:
			why = "jobs waiting but this idle runner hasn't taken one"
		case s.now().Sub(r.dispatched) > s.MaxDuration:
			why = "runner exceeded max duration"
		default:
			continue
		}
		s.log.Warn(why+", stopping", "runner", r.name, "max_duration", s.MaxDuration)
		if err := s.nomad.Stop(ctx, r.jobID); err != nil {
			s.log.Error("stop runner failed", "runner", r.name, "error", err)
		}
	}
	return s.scaleUp(ctx)
}

// Adopt records a runner dispatched by a previous narc process.
func (s *Scaler) Adopt(c *api.JobListStub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := c.Meta["runner_name"]
	s.runners[name] = &runner{name: name, jobID: c.ID, dispatched: time.Unix(0, c.SubmitTime), adopted: true}
	s.log.Info("adopted running runner", "runner", name, "nomad_job", c.ID)
	s.updateGauge()
}

func (s *Scaler) updateGauge() {
	var starting, idle, busy float64
	for _, r := range s.runners {
		switch {
		case r.busy:
			busy++
		case r.started:
			idle++
		default:
			starting++
		}
	}
	runnersByState.WithLabelValues(s.Target, s.Name, "starting").Set(starting)
	runnersByState.WithLabelValues(s.Target, s.Name, "idle").Set(idle)
	runnersByState.WithLabelValues(s.Target, s.Name, "busy").Set(busy)
}

// owns reports whether a dispatched child's meta names this scale set.
// Children dispatched before narc set narc_target are matched by the scale
// set ID in the runner name, which is ambiguous across targets.
func (s *Scaler) owns(meta map[string]string) bool {
	if t, ok := meta["narc_target"]; ok {
		return t == s.Target && meta["narc_scaleset"] == s.Name
	}
	id, ok := parseRunnerName(meta["runner_name"])
	return ok && id == s.ID
}

// Recover rebuilds state for one runner job on startup: it adopts live
// dispatched children into their scale sets and sweeps JIT config variables
// with no live child. It must run before any listener dispatches.
func Recover(ctx context.Context, nomad Nomad, job string, scalers []*Scaler, log *slog.Logger) error {
	children, err := nomad.Children(ctx, job)
	if err != nil {
		return fmt.Errorf("list children of %s: %w", job, err)
	}
	live := map[string]bool{}
	for _, c := range children {
		name := c.Meta["runner_name"]
		if c.Status == "dead" || name == "" {
			continue
		}
		live[name] = true
		var owners []*Scaler
		for _, s := range scalers {
			if s.Job == job && s.owns(c.Meta) {
				owners = append(owners, s)
			}
		}
		switch len(owners) {
		case 1:
			owners[0].Adopt(c)
		case 0:
			log.Warn("live runner belongs to no configured scale set; leaving it to finish", "nomad_job", c.ID, "runner", name)
		default:
			log.Warn("live runner has no narc_target meta and matches several scale sets; leaving it to finish", "nomad_job", c.ID, "runner", name)
		}
	}

	prefix := "nomad/jobs/" + job + "/"
	paths, err := nomad.ListVars(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list variables under %s: %w", prefix, err)
	}
	for _, p := range paths {
		name := strings.TrimPrefix(p, prefix)
		if _, ok := parseRunnerName(name); !ok || live[name] {
			continue // not a narc JIT variable, or still needed
		}
		if err := nomad.DeleteVar(ctx, p); err != nil {
			return fmt.Errorf("sweep %s: %w", p, err)
		}
		variableSweeps.Inc()
		log.Info("swept orphaned JIT config variable", "path", p)
	}
	return nil
}
