# Config for the integration tests' `nomad agent -dev`.
ports {
  http = 14646
  rpc  = 14647
  serf = 14648
}
plugin "raw_exec" {
  config {
    enabled = true
  }
}
client {
  # macOS fingerprints little or no CPU; give the scheduler something to place on.
  cpu_total_compute = 4000
}
