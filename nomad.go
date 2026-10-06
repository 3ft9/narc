package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// nomadClient implements Nomad against the real API. Address and token come
// from the environment (NOMAD_ADDR, NOMAD_TOKEN), which workload identity
// provides via the task API socket.
type nomadClient struct {
	c  *api.Client
	ns string
}

func newNomadClient(namespace string) (*nomadClient, error) {
	cfg := api.DefaultConfig()
	cfg.Namespace = namespace
	c, err := api.NewClient(cfg)
	return &nomadClient{c: c, ns: namespace}, err
}

func (n *nomadClient) q(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: n.ns}).WithContext(ctx)
}

func (n *nomadClient) w(ctx context.Context) *api.WriteOptions {
	return (&api.WriteOptions{Namespace: n.ns}).WithContext(ctx)
}

func (n *nomadClient) PutVar(ctx context.Context, path string, items map[string]string) error {
	_, _, err := n.c.Variables().Create(&api.Variable{Namespace: n.ns, Path: path, Items: items}, n.w(ctx))
	return err
}

func (n *nomadClient) DeleteVar(ctx context.Context, path string) error {
	_, err := n.c.Variables().Delete(path, n.w(ctx))
	return err
}

func (n *nomadClient) ListVars(ctx context.Context, prefix string) ([]string, error) {
	vs, _, err := n.c.Variables().PrefixList(prefix, n.q(ctx))
	var paths []string
	for _, v := range vs {
		paths = append(paths, v.Path)
	}
	return paths, err
}

func (n *nomadClient) Dispatch(ctx context.Context, job, runnerName string) (string, error) {
	resp, _, err := n.c.Jobs().Dispatch(job, map[string]string{"runner_name": runnerName}, nil, "", n.w(ctx))
	if err != nil {
		return "", err
	}
	return resp.DispatchedJobID, nil
}

func (n *nomadClient) Children(ctx context.Context, job string) ([]*api.JobListStub, error) {
	q := n.q(ctx)
	q.Prefix = job + "/dispatch-"
	jobs, _, err := n.c.Jobs().ListOptions(&api.JobListOptions{Fields: &api.JobListFields{Meta: true}}, q)
	return slices.DeleteFunc(jobs, func(j *api.JobListStub) bool { return j.ParentID != job }), err
}

func (n *nomadClient) Allocs(ctx context.Context, jobID string) ([]*api.AllocationListStub, error) {
	allocs, _, err := n.c.Jobs().Allocations(jobID, false, n.q(ctx))
	return allocs, err
}

// Stop stops a dispatched runner's allocations, which needs only
// alloc-lifecycle. Stopping a job that was never placed needs submit-job,
// which narc's policy deliberately doesn't grant; that call is then refused
// and the job stays pending until placed (and then stopped) or removed by hand.
func (n *nomadClient) Stop(ctx context.Context, jobID string) error {
	allocs, err := n.Allocs(ctx, jobID)
	if err != nil {
		return err
	}
	stopped := false
	for _, a := range allocs {
		if a.ClientStatus == api.AllocClientStatusPending || a.ClientStatus == api.AllocClientStatusRunning {
			if _, err := n.c.Allocations().Stop(&api.Allocation{ID: a.ID, Namespace: n.ns}, n.q(ctx)); err != nil {
				return err
			}
			stopped = true
		}
	}
	if !stopped {
		_, _, err = n.c.Jobs().Deregister(jobID, false, n.w(ctx))
	}
	return err
}

// CheckRunnerJob fetches a runner job and checks it against the runner job
// contract.
func (n *nomadClient) CheckRunnerJob(ctx context.Context, job string) error {
	j, _, err := n.c.Jobs().Info(job, n.q(ctx))
	if err != nil {
		return fmt.Errorf("read runner job %s: %w", job, err)
	}
	return checkRunnerJob(j)
}

func checkRunnerJob(j *api.Job) error {
	var errs []error
	if j.Type == nil || *j.Type != "batch" {
		errs = append(errs, errors.New("must be a batch job"))
	}
	if p := j.ParameterizedJob; p == nil {
		errs = append(errs, errors.New("must be parameterized"))
	} else if !slices.Contains(p.MetaRequired, "runner_name") && !slices.Contains(p.MetaOptional, "runner_name") {
		errs = append(errs, errors.New(`parameterized block must accept meta "runner_name"`))
	}
	for _, tg := range j.TaskGroups {
		name := "group " + deref(tg.Name)
		if rp := tg.RestartPolicy; rp == nil || deref(rp.Attempts) != 0 {
			errs = append(errs, fmt.Errorf("%s: restart attempts must be 0 (a JIT config is single-use)", name))
		}
		if rp := tg.ReschedulePolicy; rp == nil || deref(rp.Attempts) != 0 || deref(rp.Unlimited) {
			errs = append(errs, fmt.Errorf("%s: reschedule must be attempts = 0, unlimited = false", name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("runner job %s breaks the runner job contract: %w", deref(j.ID), errors.Join(errs...))
	}
	return nil
}

func deref[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

// Watch follows the allocation event stream and hands each allocation update
// to the scale set that owns it. It reconnects until ctx is cancelled.
func (n *nomadClient) Watch(ctx context.Context, scalers []*Scaler, log *slog.Logger) {
	for ctx.Err() == nil {
		err := n.watch(ctx, scalers)
		if ctx.Err() != nil {
			return
		}
		log.Warn("event stream ended, reconnecting", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (n *nomadClient) watch(ctx context.Context, scalers []*Scaler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := n.c.EventStream().Stream(ctx, map[api.Topic][]string{api.TopicAllocation: {"*"}}, 0, n.q(ctx))
	if err != nil {
		return err
	}
	for evs := range ch {
		if evs.Err != nil {
			return evs.Err
		}
		for _, e := range evs.Events {
			a, err := e.Allocation()
			if err != nil || a == nil {
				continue
			}
			for _, s := range scalers {
				if s.Observe(ctx, a.JobID, a.ClientStatus, a.TaskStates) {
					break
				}
			}
		}
	}
	return errors.New("stream closed")
}

// Owned scale sets are recorded in a Nomad Variable as {target URL: [IDs]},
// because the scale set API has no way to list or tag them.
func (n *nomadClient) ReadOwned(ctx context.Context, path string) (map[string][]int, error) {
	owned := map[string][]int{}
	v, _, err := n.c.Variables().Read(path, n.q(ctx))
	if errors.Is(err, api.ErrVariablePathNotFound) {
		return owned, nil
	}
	if err != nil {
		return nil, err
	}
	return owned, json.Unmarshal([]byte(v.Items["owned"]), &owned)
}

func (n *nomadClient) WriteOwned(ctx context.Context, path string, owned map[string][]int) error {
	b, err := json.Marshal(owned)
	if err != nil {
		return err
	}
	return n.PutVar(ctx, path, map[string]string{"owned": string(b)})
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "404")
}
