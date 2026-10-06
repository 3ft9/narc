# narc

**Nomad Action Runners Coordinator.** An autoscaler for ephemeral GitHub Actions self-hosted runners on [Nomad](https://www.nomadproject.io/), doing the job [actions-runner-controller](https://github.com/actions/actions-runner-controller) does on Kubernetes.

narc registers runner scale sets with GitHub, long-polls the scale set API for demand (via [`actions/scaleset`](https://github.com/actions/scaleset)), and dispatches one Nomad parameterized job per runner with a single-use just-in-time (JIT) config. Each runner takes one job and exits. The reference runner job boots a fresh QEMU/KVM VM per job, with root and a real Docker daemon inside, the same model as GitHub-hosted runners.

> [!WARNING]
> **Runner versions.** The reference job installs the latest `actions/runner` release on every boot. You can pin a version instead (`runner_version`), but if you do, you have to keep the pin current. GitHub refuses runners that fall too far behind its latest release, so a stale pin will eventually stop jobs from running, with little warning.

## How it works

```
GitHub scale set API ──long poll──▶ narc ──dispatch──▶ Nomad parameterized job ──▶ QEMU VM ──▶ run.sh --jitconfig
                                     │                       ▲
                                     └─ Nomad Variable ──────┘ (JIT config, deleted when the allocation ends)
```

- **One scale set per runner shape.** A workflow's `runs-on` picks a scale set by name, and each scale set dispatches one Nomad job. Constraints, resources, image and driver live in that job. JIT runners aren't bound to a job, so per-job placement from labels isn't possible.
- **Demand.** For each scale set narc keeps `min(max, assigned jobs + warm)` runners alive. It never scales down: runners exit when their job is done, and warm runners wait for one.
- **JIT config delivery.** narc writes the JIT config to the Nomad Variable `nomad/jobs/<runner job>/<runner name>` *before* dispatching, with meta `runner_name`. The runner job's template renders it into the VM's cloud-init seed. narc deletes the variable when the allocation ends, or if the dispatch fails. Not sooner: a restarted Nomad agent renders the task's templates again (see the runner job contract).
- **No retries.** A JIT config is single-use, so runner jobs must not restart or reschedule. If an allocation ends before its runner completes a job (VM crash, failed boot, OOM kill), narc removes the runner registration from GitHub.
- **Max duration.** narc stops any runner older than `max_duration` (default 6h15m: GitHub's 6 h job limit plus a margin).
- **Stateless.** On startup narc adopts live dispatched children of its runner jobs, sweeps JIT config variables that have no live child, and reconciles against GitHub's statistics. Jobs queue at GitHub while narc is down; running runners are unaffected.
- **Scale set lifecycle.** On startup narc creates or updates every configured scale set, and deletes scale sets *it created* that are no longer configured. It records what it created in the Nomad Variable `narc/state`, because the scale set API can't list or tag them. Scale sets narc didn't create are never deleted.

## Requirements

**Nomad clients that run runner VMs** need:

- `/dev/kvm` (bare metal, or nested virtualisation).
- QEMU on the Nomad agent's `PATH` (`qemu-system-x86_64`). The built-in `qemu` driver is detected automatically.
- CNI plugins and the `cni/narc` network: [`deploy/narc.conflist`](deploy/narc.conflist) in the client's `cni_config_dir`.
- The egress firewall: [`deploy/narc.nft`](deploy/narc.nft), with `node_addrs` filled in with every node's public addresses.
- A host volume `narc-images` for the per-node image cache (for example `/var/lib/narc/images`).
- The Docker driver, for the reference job's prestart task.

On NixOS, the flake's module does all of that except KVM. It works with either firewall: with `networking.nftables.enable` the egress rules are one of its tables; otherwise (the iptables-based `networking.firewall` that Docker and Tailscale expect) a `narc-egress` unit loads them as a separate `inet narc` table before Nomad starts. It also keeps dhcpcd off the bridge.

```nix
{
  inputs.narc.url = "github:3ft9/narc";
  # ...
  imports = [ narc.nixosModules.node ];
  services.narc-node = {
    enable = true;
    nodeAddresses = [ "203.0.113.1" "203.0.113.2" "203.0.113.3" ];
  };
}
```

If the qemu plugin has an `args_allowlist`, it must allow `-cpu`, `-smp`, `-m`, `-nic` and `-drive`.

**The Nomad cluster** needs ACLs with workload identity (Nomad 1.7+; tested against 1.11 and 2.0).

## Setup

### 1. Create a GitHub App

Create one App and install it on every org and account narc serves. Under **Settings → Developer settings → GitHub Apps → New GitHub App**:

- Webhook: off. narc doesn't need public ingress.
- Repository permissions: **Administration: read and write** (repo-level runners), **Metadata: read**.
- Organization permissions: **Self-hosted runners: read and write** (org-level runners).
- Generate a private key, and note the **Client ID**.
- Install the App on the org (all repos, or selected) and on the personal account for any personal repos. The installation ID is the number at the end of the installation's settings URL.

A personal access token also works (classic: `repo` for repo targets, `admin:org` for org targets), but the App is preferred.

Personal accounts can't have account-wide runners: each personal repo needs its own `[[target]]`. One App installed on both an org and a personal account serves every target: use the same `app_client_id` and private key, with each installation's own `installation_id`.

### 2. Register the runner job

Edit the variables at the top of [`jobs/narc-runner-qemu.nomad.hcl`](jobs/narc-runner-qemu.nomad.hcl), at least `image_sha256` (from the image release's `SHA256SUMS`). Then:

```sh
nomad job run jobs/narc-runner-qemu.nomad.hcl
nomad acl policy apply -namespace default -job narc-runner-qemu \
  narc-runner-qemu policies/narc-runner.policy.hcl
```

The job-attached policy lets the runner job's template read its per-runner variables. Implicit workload identity access covers only exact paths, not sub-paths.

### 3. Run narc

```sh
nomad var put nomad/jobs/narc app_private_key=@app.pem
nomad acl policy apply -namespace default -job narc narc policies/narc.policy.hcl
nomad job run jobs/narc.nomad.hcl
```

Edit the config in the template in [`jobs/narc.nomad.hcl`](jobs/narc.nomad.hcl) first. Changing the config means re-running the job, which restarts narc. There's no hot reload.

### 4. Use it

```yaml
jobs:
  build:
    runs-on: nomad-linux
```

If the CI that deploys narc (or its runner jobs, or the nodes) runs on narc, a broken narc can't deploy its own fix. Keep those workflows on a runner label narc doesn't serve, such as `ubuntu-latest`, or be ready to deploy by hand.

## Configuration

TOML. A full example with comments is in [`examples/narc.toml`](examples/narc.toml).

| Key | Default | |
|---|---|---|
| `listen` | `:9090` | Address for `/metrics` and `/healthz`. |
| `nomad.namespace` | `default` | Namespace of the runner jobs. Address and token come from `NOMAD_ADDR`/`NOMAD_TOKEN` (workload identity). |
| `nomad.state_variable` | `narc/state` | Variable recording the scale sets narc created. |
| `target.url` | required | `https://github.com/<org>` or `https://github.com/<owner>/<repo>`. GHES URLs work too. |
| `target.auth.app_client_id`, `installation_id`, `private_key_file` | | GitHub App credentials. |
| `target.auth.token_file` | | PAT, instead of an App. |
| `target.scaleset.name` | required | Scale set name; what `runs-on` uses. Unique per target. |
| `target.scaleset.job` | required | Parameterized Nomad job to dispatch. Several scale sets may share one. |
| `target.scaleset.max` | required | Most runners alive at once (≥ 1). |
| `target.scaleset.warm` | `0` | Idle runners kept booted. Counts towards `max`. |
| `target.scaleset.labels` | `[name]` | Scale set labels. |
| `target.scaleset.runner_group` | `default` | GitHub runner group. |
| `target.scaleset.max_duration` | `6h15m` | Runners older than this are stopped. |

## Runner job contract

narc is driver-agnostic: any job meeting this contract works. A runner job must:

- be a **parameterized batch job** accepting meta `runner_name`, `narc_target` and `narc_scaleset` (the last two tell a restarted narc which scale set owns a runner, since several targets may share a job)
- render the JIT config from `nomad/jobs/<parent job>/<runner_name>` (key `jitconfig`) with **`change_mode = "noop"`** and a plain `nomadVar`
  ```hcl
  {{ with nomadVar (printf "nomad/jobs/%s/%s" (env "NOMAD_JOB_PARENT_ID") (env "NOMAD_META_runner_name")) }}{{ .jitconfig }}{{ end }}
  ```
  `nomadVar` waits until the variable exists, and narc keeps it until the allocation ends, because a restarted Nomad agent restores the task and renders its templates again. Don't guard it with `nomadVarExists`: that doesn't wait, so it re-renders the file without the JIT config as soon as the variable is deleted. QEMU's `vvfat` reads seed files lazily, so the VM can see the emptied file.
- set **`restart { attempts = 0 }`** and **`reschedule { attempts = 0, unlimited = false }`**
- run `run.sh --jitconfig …` and exit when the runner exits
- not expose workload identity to the task
- join the `cni/narc` network (or provide equivalent egress isolation)

If a deploy tool manages your Nomad jobs, it must ignore dispatched children (`<job>/dispatch-…`). They inherit the parent job's meta, including any ownership marker, so a tool that stops "orphaned" jobs carrying its marker will stop live runners. Also, registering a parameterized job returns no evaluation ID, so don't wait on one.

narc checks the job type, parameterization and retry settings before starting each scale set's listener, and won't start a scale set whose job breaks them. It rechecks on every retry, so fixing the job is enough.

### The reference QEMU job

[`jobs/narc-runner-qemu.nomad.hcl`](jobs/narc-runner-qemu.nomad.hcl):

1. **Prestart** (`narc-image-fetch`, in the narc image): if the base image isn't in the node cache, downloads it, checks its SHA-256 and atomically moves it into place, under a lock. Then creates a per-allocation qcow2 overlay of `disk_size` in the alloc dir. Only the first job on each node pays for the download.
2. **VM**: boots the overlay under KVM, with a cloud-init NoCloud seed (`user-data`, `meta-data`, `network-config`) rendered into `secrets/seed` and attached with QEMU's `vvfat` as a FAT disk labelled `CIDATA`.
3. **cloud-init** installs Docker and the runner, runs the runner as user `runner` (passwordless sudo, in group `docker`), then powers off, which ends the task.
4. **Poststop** deletes the overlay once the VM has exited. The overlay holds everything the job wrote (several GB for a typical build), and a dead allocation's directory stays on disk until the Nomad client garbage-collects it (see the client's `gc_*` settings), so without this a busy node's disk fills within hours.

`user-data` is where you customise the VM: extra packages and setup steps. Repos that need a specific environment should use [job containers](https://docs.github.com/en/actions/using-jobs/running-jobs-in-a-container) (`container:`), which work because the VM has a real Docker daemon. VM images are per scale set, never per repo.

Any image that boots under KVM, reads a NoCloud seed (or mounts the `CIDATA` disk itself), runs the runner, and powers off afterwards will work. Set `image_url` and `image_sha256`.

## Security model

- **Untrusted code runs in a VM.** Each job gets a fresh VM behind a hardware virtualisation boundary, with a copy-on-write disk deleted with the allocation. Nothing persists between jobs. The workload never gets privileges on the node.
- **Internet egress only.** Runner VMs sit on their own CNI bridge. The nftables rules drop traffic from it to RFC 1918, CGNAT/tailnet (`100.64.0.0/10`), link-local and metadata, loopback, IPv6 ULA and link-local, every node's public addresses, and the node itself. No CI job can reach Nomad, Consul, Vault or anything else in the cluster. Guests use public DNS resolvers.
- **The JIT config is a credential.** Whoever reads it first can take the runner's job, secrets included. It never appears in dispatch meta or payloads, which anyone with `read-job` can see. It lives in a Nomad Variable until the runner's allocation ends, readable only by the runner job's template (narc can write, list and delete it, not read it). It's also in the task's `secrets/` dir (not exposed by `nomad alloc fs`) and the VM's seed disk. By the time any workflow step runs, it has been used.
- **Least-privilege Nomad access.** narc uses workload identity with no long-lived token. Its policy grants `list-jobs`, `read-job`, `dispatch-job` and `alloc-lifecycle`, plus variables under the runner jobs' prefixes. It deliberately doesn't grant `submit-job`, which would let a compromised narc register arbitrary jobs. As a result, narc can't stop a dispatched runner that never got placed; that job stays pending until it's placed (and then stopped) or removed by hand.
- **Sibling visibility.** Each dispatch of a runner job could technically read every sibling's variable, but only through its template, which the jobspec controls. The guest has no Nomad token.
- **Untrusted pull requests.** The VM boundary protects the cluster, not your secrets. GitHub's usual rules about running workflows from forks still apply.

## Observability

- `/metrics` (Prometheus):
  - `narc_runners{target,scaleset,state}`: live runners: `starting`, `idle` (booted, waiting) and `busy`
  - `narc_github_assigned_jobs`, `narc_github_running_jobs`: from GitHub's statistics
  - `narc_dispatch_failures_total`
  - `narc_boot_to_job_start_seconds`: dispatch to job start
  - `narc_runner_deregistrations_total`: runners removed because their allocation ended without completing a job
  - `narc_variable_sweeps_total`: orphaned JIT config variables removed on startup
- `/healthz`: 200 once every scale set has an active message session, otherwise 503.
- JSON logs on stdout.

## Testing

```sh
go test ./...                                         # unit tests
go test -tags integration -run Integration -v ./...   # needs `nomad` on PATH
```

The integration test starts an ACL-enabled single-node Nomad agent on port 14646. It applies the shipped policies to a stub `raw_exec` runner job and a narc token, then runs a runner through dispatch, completion, variable deletion, deregistration, restart adoption and max-duration stop. It also restarts the agent under a live runner, and checks that the runner still has its JIT config and that its allocation completes. Missing ACL capabilities fail it.

### End to end

This needs real GitHub credentials, so it doesn't run in CI.

1. Create a test repo, and install the GitHub App on it (or use a PAT).
2. On a KVM node with the requirements above, register the runner job and run narc with a target for the test repo and a scale set, for example `narc-e2e` with `max = 1`.
3. Check `/healthz` returns 200, and that the scale set appears under the repo's **Settings → Actions → Runners**.
4. Push a workflow with `runs-on: narc-e2e` that runs `docker run --rm hello-world` and `sudo true`.
5. Watch for: a dispatched `narc-runner-qemu/dispatch-…` job; `nomad var list nomad/jobs/narc-runner-qemu/` showing the runner's variable; the job passing; the VM powering off, the allocation completing and the variable gone; `narc_runners` back to `warm`.
6. Kill a VM mid-job (`nomad alloc stop`) and check that the runner disappears from GitHub and `narc_runner_deregistrations_total` increases.
7. Remove the scale set from the config, restart narc, and check that it's deleted on GitHub.

## Not yet done

- **Image cache garbage collection (required).** Cached base images are never removed when `image_sha256` changes, so node disks fill up over time. `narc-image-fetch` touches each image's mtime on use, so a GC can remove images unused for N days.
- Podman reference job for trusted, Docker-free workloads.
- Per-node image preparation (bake the runner and Docker in once per node) if boot time hurts.
- Out of scope: HA/leader election (GitHub allows one session per scale set anyway), hot config reload, Firecracker, runner container hooks, an in-cluster image mirror.

## Images and releases

Every push to `main` publishes `ghcr.io/3ft9/narc:latest` and `ghcr.io/3ft9/narc:sha-<short commit>`. Pin deployments to a `sha-` tag (`-var image=…` on `narc.nomad.hcl`, `-var prestart_image=…` on the runner job) so you know what's running. Tagging `vX.Y.Z` publishes a versioned image, linux binaries and checksums via GoReleaser.

## Licence

MIT. See [LICENSE](LICENSE).
