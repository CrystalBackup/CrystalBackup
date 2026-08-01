# ---------------------------------------------------------------------------
# Nodes: 3 masters (etcd + control plane + ceph mon/mgr)
#        3 workers (osd/mds/rgw/longhorn) with two extra raw volumes each
#                  (/dev/sdb -> ceph OSD, /dev/sdc -> node-local CSI drivers)
# ---------------------------------------------------------------------------

locals {
  # Deterministic private IPs — .1 is the hcloud gateway.
  master_ips = { for i in range(var.master_count) : i => cidrhost(var.subnet_cidr, 11 + i) }
  worker_ips = { for i in range(var.worker_count) : i => cidrhost(var.subnet_cidr, 21 + i) }
}

resource "hcloud_server" "master" {
  count = var.master_count

  name               = "${local.name_prefix}-master-${count.index + 1}"
  server_type        = var.master_type
  image              = var.image
  location           = var.location
  ssh_keys           = [data.hcloud_ssh_key.crucible.id]
  firewall_ids       = [hcloud_firewall.crucible.id]
  placement_group_id = hcloud_placement_group.crucible.id
  labels             = merge(local.lane_labels, { role = "master" })

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

  name               = "${local.name_prefix}-worker-${count.index + 1}"
  server_type        = var.worker_type
  image              = var.image
  location           = var.location
  ssh_keys           = [data.hcloud_ssh_key.crucible.id]
  firewall_ids       = [hcloud_firewall.crucible.id]
  placement_group_id = hcloud_placement_group.crucible.id
  labels             = merge(local.lane_labels, { role = "worker" })

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

  name      = "${local.name_prefix}-ceph-${count.index + 1}"
  size      = var.ceph_volume_size
  location  = var.location
  labels    = merge(local.lane_labels, { role = "ceph-osd" })
  format    = null # raw on purpose — rook formats it
  automount = false
}

resource "hcloud_volume_attachment" "ceph" {
  count = var.worker_count

  volume_id = hcloud_volume.ceph[count.index].id
  server_id = hcloud_server.worker[count.index].id
  automount = false
}

# Second raw, UNFORMATTED volume per worker — the device the node-local CSI
# drivers under test consume (OpenEBS LocalPV LVM, TopoLVM, OpenEBS LocalPV ZFS,
# LINSTOR). It lands as /dev/sdc, which is exactly why rook's deviceFilter is
# pinned to `^sdb$`: widen it and Ceph eats this disk before any driver sees it.
# Partitioned and prepared by the `storage_prep` ansible role, not here.
resource "hcloud_volume" "csi" {
  count = var.worker_count

  name      = "${local.name_prefix}-csi-${count.index + 1}"
  size      = var.extra_volume_size
  location  = var.location
  labels    = merge(local.lane_labels, { role = "csi-extra" })
  format    = null # raw on purpose — storage_prep partitions it
  automount = false
}

resource "hcloud_volume_attachment" "csi" {
  count = var.worker_count

  volume_id = hcloud_volume.csi[count.index].id
  server_id = hcloud_server.worker[count.index].id
  automount = false

  # Attach the Ceph disk FIRST so the kernel enumerates it as sdb and this one
  # as sdc. Hetzner names nothing — the guest does, in attach order — so without
  # this the two can swap and rook's `^sdb$` filter would aim at the wrong disk.
  depends_on = [hcloud_volume_attachment.ceph]
}
