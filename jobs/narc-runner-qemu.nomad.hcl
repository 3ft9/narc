# Reference runner job: one ephemeral GitHub Actions runner in a QEMU/KVM VM.
#
# narc dispatches this job once per runner with meta runner_name, after
# writing the runner's JIT config to nomad/jobs/<this job>/<runner_name>.
#
# Register it, then attach its ACL policy so its template can read the
# per-runner variables:
#
#   nomad job run jobs/narc-runner-qemu.nomad.hcl
#   nomad acl policy apply -namespace default -job narc-runner-qemu \
#     narc-runner-qemu policies/narc-runner.policy.hcl
#
# One runner job per runner shape. Copy it under a new name for a different
# size, image or placement, and point a scale set's `job` at it.

variable "image_url" {
  description = "Base image URL. Pin a dated release, not current/."
  default     = "https://cloud-images.ubuntu.com/releases/noble/release-20260926/noble-server-cloudimg-amd64.img"
}

variable "image_sha256" {
  description = "SHA-256 of the base image, from the release's SHA256SUMS."
  default     = "REPLACE_WITH_SHA256_FROM_SHA256SUMS"
}

variable "disk_size" {
  description = "Size of each VM's root disk (a copy-on-write overlay); cloud-init grows the filesystem to fit."
  default     = "40G"
}

variable "cpus" {
  default = 4
}

variable "memory_mb" {
  default = 8192
}

variable "runner_version" {
  description = <<-EOT
    actions/runner version, or "latest". If you pin, you must keep the pin
    current: GitHub refuses runners that fall too far behind its latest release.
  EOT
  default     = "latest"
}

variable "prestart_image" {
  description = "Image with narc-image-fetch, curl and qemu-img. The narc image has them."
  default     = "ghcr.io/3ft9/narc:latest"
}

job "narc-runner-qemu" {
  type = "batch"

  parameterized {
    meta_required = ["runner_name"]
  }

  group "runner" {
    # A JIT config is single-use: a retried allocation can't register.
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }

    # Internet egress only; see deploy/narc.conflist and deploy/narc.nft.
    network {
      mode = "cni/narc"
      # Docker gives fetch-image the host's resolv.conf, which usually names a
      # loopback resolver (systemd-resolved, dnsmasq) that is unreachable from
      # this network namespace, and narc.nft drops traffic to the node anyway.
      # Use public resolvers, like the VM does.
      dns {
        servers = ["1.1.1.1", "9.9.9.9"]
      }
    }

    ephemeral_disk {
      # The overlay grows as the VM writes; this is what the scheduler reserves.
      size = 20480
    }

    volume "images" {
      type   = "host"
      source = "narc-images"
    }

    task "fetch-image" {
      lifecycle {
        hook = "prestart"
      }

      driver = "docker"
      user   = "root" # writes to the root-owned cache volume
      config {
        image      = var.prestart_image
        entrypoint = ["/usr/local/bin/narc-image-fetch"]
      }

      # Same path as on the host: the overlay stores the backing file's path.
      volume_mount {
        volume      = "images"
        destination = "/var/lib/narc/images"
      }

      env {
        IMAGE_URL    = var.image_url
        IMAGE_SHA256 = var.image_sha256
        DISK_SIZE    = var.disk_size
        CACHE_DIR    = "/var/lib/narc/images"
      }

      resources {
        cpu    = 200
        memory = 128
      }
    }

    task "vm" {
      driver = "qemu"

      # The guest has no Nomad token, and neither does anything on the host
      # side of this task.
      identity {
        env  = false
        file = false
      }

      config {
        image_path  = "../alloc/disk.qcow2"
        accelerator = "kvm"
        args = [
          "-cpu", "host",
          "-smp", "${var.cpus}",
          "-nic", "user,model=virtio-net-pci",
          # cloud-init NoCloud seed: secrets/seed as a FAT disk labelled CIDATA.
          "-drive", "driver=vvfat,dir=${NOMAD_SECRETS_DIR}/seed,label=CIDATA,if=virtio,read-only=on",
        ]
      }

      template {
        destination = "secrets/seed/meta-data"
        change_mode = "noop"
        data        = <<-EOT
          instance-id: {{ env "NOMAD_ALLOC_ID" }}
          local-hostname: {{ env "NOMAD_META_runner_name" }}
        EOT
      }

      # QEMU's user-mode DNS forwards to the host's resolver, which is
      # cluster-internal and blocked. Point the guest at public resolvers.
      template {
        destination = "secrets/seed/network-config"
        change_mode = "noop"
        data        = <<-EOT
          version: 2
          ethernets:
            nic:
              match: { name: "e*" }
              dhcp4: true
              dhcp4-overrides: { use-dns: false }
              nameservers: { addresses: [1.1.1.1, 9.9.9.9] }
        EOT
      }

      # The whole user-data, with the JIT config inlined. change_mode must be
      # noop: narc deletes the variable once this task starts, and the default
      # (restart) would restart the VM.
      template {
        destination = "secrets/seed/user-data"
        change_mode = "noop"
        data        = <<-EOT
          #cloud-config
          users:
            - default
            - name: runner
              groups: [docker, sudo]
              sudo: "ALL=(ALL) NOPASSWD:ALL"
              shell: /bin/bash
          package_update: true
          packages: [docker.io, curl, jq, git, unzip, zip, build-essential]
          write_files:
            - path: /run/gha/jitconfig
              permissions: "0600"
              content: "{{ with nomadVar (printf "nomad/jobs/%s/%s" (env "NOMAD_JOB_PARENT_ID") (env "NOMAD_META_runner_name")) }}{{ .jitconfig }}{{ end }}"
          runcmd:
            - - sh
              - -c
              - |
                set -eu
                v="${var.runner_version}"
                if [ "$v" = latest ]; then
                  # The redirect, not the REST API: no 60/hour rate limit.
                  v=$(curl -fsSLI -o /dev/null -w '%%{url_effective}' https://github.com/actions/runner/releases/latest | sed 's|.*/v||')
                fi
                case "$(uname -m)" in aarch64) arch=arm64 ;; *) arch=x64 ;; esac
                mkdir -p /opt/runner
                curl -fsSL "https://github.com/actions/runner/releases/download/v$v/actions-runner-linux-$arch-$v.tar.gz" | tar xz -C /opt/runner
                /opt/runner/bin/installdependencies.sh
                chown -R runner:runner /opt/runner
                jit=$(cat /run/gha/jitconfig)
                rm -f /run/gha/jitconfig
                cd /opt/runner
                sudo -u runner ./run.sh --jitconfig "$jit" || true
          # Power off when the runner exits (or setup fails), ending the task.
          power_state:
            mode: poweroff
            condition: true
        EOT
      }

      kill_timeout = "30s"

      resources {
        cpu    = var.cpus * 1000
        memory = var.memory_mb
      }
    }
  }
}
