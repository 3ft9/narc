# Workload identity policy for the narc service job.
#
#   nomad acl policy apply -namespace default -job narc narc policies/narc.policy.hcl
#
# Add a variables path block per runner job if you run more than one.
namespace "default" {
  # read-job: job/allocation status and the allocation event stream.
  # dispatch-job: start runners. alloc-lifecycle: stop runners over max duration.
  # Deliberately not submit-job, which would let narc register arbitrary jobs.
  capabilities = ["list-jobs", "read-job", "dispatch-job", "alloc-lifecycle"]

  variables {
    # Per-runner JIT configs.
    path "nomad/jobs/narc-runner-qemu/*" {
      capabilities = ["write", "destroy", "list"]
    }
    # Record of scale sets narc created (state_variable).
    path "narc/*" {
      capabilities = ["read", "write"]
    }
  }
}
