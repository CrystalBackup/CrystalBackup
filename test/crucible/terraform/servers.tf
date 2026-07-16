# ---------------------------------------------------------------------------
# Nodes: 3 masters (etcd + control plane + ceph mon/mgr)
#        3 workers (osd/mds/rgw/longhorn) with an extra raw volume each
# ---------------------------------------------------------------------------

locals {
  # Deterministic private IPs — .1 is the hcloud gateway.
  master_ips = { for i in range(var.master_count) : i => cidrhost(var.subnet_cidr, 11 + i) }
  worker_ips = { for i in range(var.worker_count) : i => cidrhost(var.subnet_cidr, 21 + i) }
}

resource "hcloud_server" "master" {
  count = var.master_count

  name               = "${var.name_prefix}-master-${count.index + 1}"
  server_type        = var.master_type
  image              = var.image
  location           = var.location
  ssh_keys           = [data.hcloud_ssh_key.crucible.id]
  firewall_ids       = [hcloud_firewall.crucible.id]
  placement_group_id = hcloud_placement_group.crucible.id
  labels             = merge(var.labels, { role = "master" })

  public_net {
    ipv4_enabled = true
    ipv6_enabled = false
  }

  network {
    network_id = hcloud_network.crucible.id
    ip         = local.master_ips[count.index]
  }

  depends_on = [hcloud_network_subnet.nodes]

  lifecycle {
    ignore_changes = [image] # image slug updates must not rebuild a live cluster
  }
}

resource "hcloud_server" "worker" {
  count = var.worker_count

  name               = "${var.name_prefix}-worker-${count.index + 1}"
  server_type        = var.worker_type
  image              = var.image
  location           = var.location
  ssh_keys           = [data.hcloud_ssh_key.crucible.id]
  firewall_ids       = [hcloud_firewall.crucible.id]
  placement_group_id = hcloud_placement_group.crucible.id
  labels             = merge(var.labels, { role = "worker" })

  public_net {
    ipv4_enabled = true
    ipv6_enabled = false
  }

  network {
    network_id = hcloud_network.crucible.id
    ip         = local.worker_ips[count.index]
  }

  depends_on = [hcloud_network_subnet.nodes]

  lifecycle {
    ignore_changes = [image]
  }
}

# Raw, UNFORMATTED volume per worker — future Ceph OSD device. Attached
# volumes appear as /dev/sdb (virtio-scsi); rook's deviceFilter picks them up.
resource "hcloud_volume" "ceph" {
  count = var.worker_count

  name      = "${var.name_prefix}-ceph-${count.index + 1}"
  size      = var.ceph_volume_size
  location  = var.location
  labels    = merge(var.labels, { role = "ceph-osd" })
  format    = null # raw on purpose — rook formats it
  automount = false
}

resource "hcloud_volume_attachment" "ceph" {
  count = var.worker_count

  volume_id = hcloud_volume.ceph[count.index].id
  server_id = hcloud_server.worker[count.index].id
  automount = false
}
