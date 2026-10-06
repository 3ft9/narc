# Job-attached policy for a runner job, so its template can read the
# per-runner JIT config at nomad/jobs/<job>/<runner_name>. Implicit job access
# covers only exact paths, not sub-paths.
#
#   nomad acl policy apply -namespace default -job narc-runner-qemu \
#     narc-runner-qemu policies/narc-runner.policy.hcl
namespace "default" {
  variables {
    path "nomad/jobs/narc-runner-qemu/*" {
      capabilities = ["read"]
    }
  }
}
