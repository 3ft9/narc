# The narc service. One instance: GitHub allows one message session per
# scale set, so replicas would only fight. Changing the config means
# re-running this job, which restarts narc.
#
# Secrets live in Nomad Variables at nomad/jobs/narc (readable by this job
# through workload identity), for example:
#
#   nomad var put nomad/jobs/narc app_private_key=@app.pem pat_stut=@pat.txt
#
# Then apply the service policy and run the job:
#
#   nomad acl policy apply -namespace default -job narc narc policies/narc.policy.hcl
#   nomad job run jobs/narc.nomad.hcl

variable "image" {
  default = "ghcr.io/3ft9/narc:latest"
}

job "narc" {
  type = "service"

  group "narc" {
    count = 1

    network {
      port "http" {
        to = 9090
      }
    }

    service {
      name     = "narc"
      port     = "http"
      provider = "consul"

      check {
        type     = "http"
        path     = "/healthz"
        interval = "15s"
        timeout  = "3s"
      }
    }

    task "narc" {
      driver = "docker"

      config {
        image = var.image
        args  = ["-config", "${NOMAD_TASK_DIR}/narc.toml"]
        ports = ["http"]
      }

      # Nomad API access via workload identity and the task API socket; no
      # long-lived token.
      identity {
        env = true
      }
      env {
        NOMAD_ADDR = "unix://${NOMAD_SECRETS_DIR}/api.sock"
      }

      template {
        destination = "secrets/app.pem"
        perms       = "0400"
        uid         = 65534 # the image's USER; templates are root-owned by default
        gid         = 65534
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/narc" }}{{ .app_private_key }}{{ end }}
        EOT
      }

      template {
        destination = "secrets/pat"
        perms       = "0400"
        uid         = 65534 # the image's USER; templates are root-owned by default
        gid         = 65534
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/narc" }}{{ .pat_stut }}{{ end }}
        EOT
      }

      # See examples/narc.toml for every option.
      template {
        destination = "local/narc.toml"
        data        = <<-EOT
          listen = ":9090"

          [nomad]
          namespace = "default"

          [[target]]
          url = "https://github.com/3ft9"
          [target.auth]
          app_client_id    = "REPLACE_ME"
          installation_id  = 0
          private_key_file = "{{ env "NOMAD_SECRETS_DIR" }}/app.pem"

          [[target.scaleset]]
          name = "nomad-linux"
          job  = "narc-runner-qemu"
          max  = 4
          warm = 1

          [[target]]
          url = "https://github.com/stut/some-repo"
          [target.auth]
          token_file = "{{ env "NOMAD_SECRETS_DIR" }}/pat"

          [[target.scaleset]]
          name = "nomad-linux"
          job  = "narc-runner-qemu"
          max  = 2
        EOT
      }

      resources {
        cpu    = 200
        memory = 128
      }
    }
  }
}
