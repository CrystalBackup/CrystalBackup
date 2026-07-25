# Lanes — several INDEPENDENT crucibles from one configuration.
#
# Running the suite N times concurrently on N clusters turns flakiness from a guess into a
# measurement: a spec that fails in one lane and passes in the other two is timing, and one that
# fails in all three is a bug. That is worth real money to learn quickly, so the mechanism exists —
# but it is strictly opt-in.
#
# The tofu WORKSPACE is the whole switch, deliberately, because it already isolates the one thing
# that actually cannot be shared: state. Deriving every other name from it means a lane cannot be
# half-applied — there is no second knob to forget.
#
#   default workspace  →  lane = ""  →  names, paths and state IDENTICAL to before lanes existed.
#   workspace "b"      →  lane = "b" →  crucible-b-* resources, artifacts/lanes/b/, its own state.
#
# The default path is byte-for-byte what it was: `mise run up` with no lane selected still writes
# artifacts/kubeconfig and creates crucible-master-1. Nothing about the single-lane workflow moves.
locals {
  # tofu has no "no workspace" state: an unselected config reports "default".
  lane = terraform.workspace == "default" ? "" : terraform.workspace

  # Every Hetzner resource name and label derives from this, so two lanes cannot collide on a
  # server name, a network, a firewall or a placement group.
  name_prefix = local.lane == "" ? var.name_prefix : "${var.name_prefix}-${local.lane}"

  # Where the kubeconfig, the inventory and the crucible.env facts land. Lanes get their own
  # subtree: a second lane overwriting artifacts/kubeconfig would silently point every kubectl in
  # the session at the wrong cluster, which is the kind of mistake that wastes an afternoon before
  # anyone suspects the harness.
  artifacts_dir = local.lane == "" ? "${path.module}/../artifacts" : "${path.module}/../artifacts/lanes/${local.lane}"

  # The Ansible inventory is a single file per lane for the same reason.
  inventory_file = local.lane == "" ? "${path.module}/../ansible/inventory/hosts.ini" : "${path.module}/../ansible/inventory/hosts-${local.lane}.ini"

  # Stamped on every resource so scripts/nuke.sh can find and destroy ONE lane without touching
  # its siblings — the state-less teardown must stay usable when a lane's state is lost, and
  # "delete everything labelled crucible" would take the other lanes down with it.
  lane_labels = merge(var.labels, local.lane == "" ? {} : { lane = local.lane })
}

output "lane" {
  description = "Which lane this state belongs to (empty for the default, single-lane crucible)."
  value       = local.lane
}
