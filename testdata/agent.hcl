# Config for the integration tests' single-node Nomad agent. Not -dev: the
# tests restart the agent, so it needs real (on-disk) server and client state.
# The tests pass -data-dir.
bind_addr = "127.0.0.1"
advertise {
  http = "127.0.0.1"
  rpc  = "127.0.0.1"
  serf = "127.0.0.1"
}
ports {
  http = 14646
  rpc  = 14647
  serf = 14648
}
acl {
  enabled = true
}
server {
  enabled          = true
  bootstrap_expect = 1
}
client {
  enabled = true
  # macOS fingerprints little or no CPU; give the scheduler something to place on.
  cpu_total_compute = 4000
}
plugin "raw_exec" {
  config {
    enabled = true
  }
}
