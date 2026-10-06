//go:build integration

// Integration tests against a real Nomad agent. Run with:
//
//	go test -tags integration -run Integration ./...
//
// They start an ACL-enabled single-node Nomad agent (server and client) on
// port 14646, so nothing else may be listening there.
package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

// A stub runner job following the runner job contract, minus the VM: it
// sleeps like a short job, then fails unless the JIT config is still in the
// rendered file. QEMU's vvfat seed disk reads files lazily, so the file must
// keep the JIT config while the task runs.
const stubRunnerJob = `
job "narc-stub-runner" {
  type = "batch"
  parameterized {
    meta_required = ["runner_name"]
    meta_optional = ["narc_target", "narc_scaleset"]
  }
  group "runner" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "runner" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 8 && grep -q '^jit-' ${NOMAD_SECRETS_DIR}/jitconfig"]
      }
      resources {
        cpu    = 20
        memory = 32
      }
      template {
        destination = "secrets/jitconfig"
        change_mode = "noop"
        data        = <<-EOT
          {{ with nomadVar (printf "nomad/jobs/%s/%s" (env "NOMAD_JOB_PARENT_ID") (env "NOMAD_META_runner_name")) }}{{ .jitconfig }}{{ end }}
        EOT
      }
    }
    task "cleanup" {
      lifecycle {
        hook = "poststop"
      }
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "true"]
      }
      resources {
        cpu    = 20
        memory = 32
      }
    }
  }
}
`

// startNomad starts an ACL-enabled agent, registers the stub runner job and
// returns a narc client limited to the shipped policies (with the runner job
// name substituted), so missing capabilities fail the test. restart kills the
// agent (tasks keep running, as in a crash or upgrade) and starts it again on
// the same state.
func startNomad(t *testing.T) (n *nomadClient, restart func()) {
	t.Helper()
	dataDir := t.TempDir()
	var cmd *exec.Cmd
	start := func() {
		cmd = exec.Command("nomad", "agent", "-config", "testdata/agent.hcl", "-data-dir", dataDir)
		if err := cmd.Start(); err != nil {
			t.Skipf("can't start nomad agent: %v", err)
		}
	}
	start()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	t.Setenv("NOMAD_ADDR", "http://127.0.0.1:14646")
	t.Setenv("NOMAD_TOKEN", "")

	admin, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var root *api.ACLToken
	waitFor(t, 30*time.Second, func() bool {
		root, _, err = admin.ACLTokens().Bootstrap(nil)
		return err == nil
	})
	admin.SetSecretID(root.SecretID)
	ready := func() bool {
		nodes, _, err := admin.Nodes().List(nil)
		return err == nil && len(nodes) > 0 && nodes[0].Status == "ready"
	}
	waitFor(t, 30*time.Second, ready)
	restart = func() {
		cmd.Process.Kill()
		cmd.Wait()
		start()
		waitFor(t, 30*time.Second, ready)
	}

	job, err := admin.Jobs().ParseHCL(stubRunnerJob, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := admin.Jobs().Register(job, nil); err != nil {
		t.Fatal(err)
	}

	policy := func(file string) string {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "narc-runner-qemu", "narc-stub-runner")
	}
	if _, err := admin.ACLPolicies().Upsert(&api.ACLPolicy{Name: "runner", Rules: policy("policies/narc-runner.policy.hcl"),
		JobACL: &api.JobACL{Namespace: "default", JobID: "narc-stub-runner"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ACLPolicies().Upsert(&api.ACLPolicy{Name: "narc", Rules: policy("policies/narc.policy.hcl")}, nil); err != nil {
		t.Fatal(err)
	}
	tok, _, err := admin.ACLTokens().Create(&api.ACLToken{Type: "client", Policies: []string{"narc"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOMAD_TOKEN", tok.SecretID)
	n, err = newNomadClient("default")
	if err != nil {
		t.Fatal(err)
	}
	return n, restart
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

func TestIntegrationRunnerLifecycle(t *testing.T) {
	ctx := t.Context()
	n, _ := startNomad(t)

	poststop, err := n.CheckRunnerJob(ctx, "narc-stub-runner")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(poststop, []string{"cleanup"}) {
		t.Fatalf("poststop = %v", poststop)
	}

	// The owned scale set record round-trips under the narc policy.
	if owned, err := n.ReadOwned(ctx, "narc/state"); err != nil || len(owned) != 0 {
		t.Fatalf("ReadOwned on missing variable = %v, %v", owned, err)
	}
	if err := n.WriteOwned(ctx, "narc/state", map[string][]int{"https://github.com/o": {1, 2}}); err != nil {
		t.Fatal(err)
	}
	if owned, err := n.ReadOwned(ctx, "narc/state"); err != nil || len(owned["https://github.com/o"]) != 2 {
		t.Fatalf("ReadOwned = %v, %v", owned, err)
	}

	// A variable left by a "previous process" is swept on recovery.
	orphan := "nomad/jobs/narc-stub-runner/nomad-linux-7-deadbeef"
	if err := n.PutVar(ctx, orphan, map[string]string{"jitconfig": "x"}); err != nil {
		t.Fatal(err)
	}

	gh := &fakeGitHub{}
	s := NewScaler(&ScaleSetConfig{Name: "nomad-linux", Job: "narc-stub-runner", Max: 2, MaxDuration: duration{time.Hour}},
		"https://github.com/o", 7, gh, n, discard)
	s.SetPoststop([]string{"cleanup"})
	if err := Recover(ctx, n, "narc-stub-runner", []*Scaler{s}, discard); err != nil {
		t.Fatal(err)
	}
	if vars, _ := n.ListVars(ctx, "nomad/jobs/narc-stub-runner/"); len(vars) != 0 {
		t.Fatalf("orphan not swept: %v", vars)
	}

	go n.Watch(ctx, []*Scaler{s}, discard)
	time.Sleep(time.Second) // let the stream subscribe

	s.HandleDesiredRunnerCount(ctx, 1)
	s.mu.Lock()
	s.demand = 0 // otherwise the freed slot is refilled
	if len(s.runners) != 1 {
		t.Fatalf("runners = %d", len(s.runners))
	}
	var r runner
	for _, v := range s.runners {
		r = *v
	}
	s.mu.Unlock()

	// The event stream sees the task start...
	waitFor(t, 30*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.runners[r.name].started
	})

	// ...and forgets the runner and deletes its variable once the allocation
	// ends. It never ran a GitHub job, so it is deregistered.
	waitFor(t, 30*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.runners) == 0
	})
	if vars, _ := n.ListVars(ctx, "nomad/jobs/narc-stub-runner/"); len(vars) != 0 {
		t.Fatalf("variable not deleted: %v", vars)
	}
	if !slices.Contains(gh.removed, r.runnerID) {
		t.Fatalf("runner not deregistered: %v", gh.removed)
	}
	allocs, _ := n.Allocs(ctx, r.jobID)
	if len(allocs) != 1 || allocs[0].ClientStatus != api.AllocClientStatusComplete {
		t.Fatalf("stub task didn't get its JIT config: %+v", allocs[0])
	}

	// A restarted narc adopts live children.
	s.HandleDesiredRunnerCount(ctx, 1)
	s.mu.Lock()
	s.demand = 0
	s.mu.Unlock()
	s2 := NewScaler(&ScaleSetConfig{Name: "nomad-linux", Job: "narc-stub-runner", Max: 2, MaxDuration: duration{time.Hour}},
		"https://github.com/o", 7, gh, n, discard)
	s2.SetPoststop([]string{"cleanup"})
	if err := Recover(ctx, n, "narc-stub-runner", []*Scaler{s2}, discard); err != nil {
		t.Fatal(err)
	}
	if len(s2.runners) != 1 {
		t.Fatalf("adopted %d runners, want 1", len(s2.runners))
	}

	// Max duration stops the job, and reconcile notices it's dead.
	s2.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s2.now = time.Now
	waitFor(t, 30*time.Second, func() bool {
		s2.Reconcile(ctx)
		s2.mu.Lock()
		defer s2.mu.Unlock()
		return len(s2.runners) == 0
	})
}

// A restarted Nomad agent re-renders the restored task's templates. The JIT
// config variable must still be there: nomadVar would block on a deleted one
// forever, and the task would never see its runner exit.
func TestIntegrationAgentRestart(t *testing.T) {
	ctx := t.Context()
	n, restart := startNomad(t)

	gh := &fakeGitHub{}
	s := NewScaler(&ScaleSetConfig{Name: "nomad-linux", Job: "narc-stub-runner", Max: 1, MaxDuration: duration{time.Hour}},
		"https://github.com/o", 7, gh, n, discard)
	s.SetPoststop([]string{"cleanup"})
	go n.Watch(ctx, []*Scaler{s}, discard)
	time.Sleep(time.Second) // let the stream subscribe

	s.HandleDesiredRunnerCount(ctx, 1)
	s.mu.Lock()
	s.demand = 0
	var r runner
	for _, v := range s.runners {
		r = *v
	}
	s.mu.Unlock()
	waitFor(t, 30*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.runners[r.name].started
	})

	restart()

	waitFor(t, 60*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.runners) == 0
	})
	if allocs, _ := n.Allocs(ctx, r.jobID); len(allocs) != 1 || allocs[0].ClientStatus != api.AllocClientStatusComplete {
		t.Fatalf("stub task lost its JIT config across the restart: %+v", allocs)
	}
}
