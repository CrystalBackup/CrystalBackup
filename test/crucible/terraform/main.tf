# ---------------------------------------------------------------------------
# Shared plumbing: SSH key, private network, firewall, placement group
# ---------------------------------------------------------------------------

# The SSH key must already exist in the Hetzner project (see README).
data "hcloud_ssh_key" "crucible" {
  name = var.ssh_key_name
}

resource "hcloud_network" "crucible" {
  name     = "${var.name_prefix}-net"
  ip_range = var.network_cidr
  labels   = var.labels
}

resource "hcloud_network_subnet" "nodes" {
  network_id   = hcloud_network.crucible.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = var.subnet_cidr
}

# hcloud firewalls only filter the PUBLIC interface; node-to-node traffic
# (etcd 2379/2380, supervisor 9345, CNI vxlan, ceph, ...) rides the private
# network and is unaffected.
resource "hcloud_firewall" "crucible" {
  name   = "${var.name_prefix}-fw"
  labels = var.labels

  rule {
    description = "SSH (ansible)"
    direction   = "in"
    protocol    = "tcp"
    port        = "22"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "Kubernetes API (kubectl / tests from the operator's machine)"
    direction   = "in"
    protocol    = "tcp"
    port        = "6443"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "HTTP ingress (rke2-ingress-nginx test traffic)"
    direction   = "in"
    protocol    = "tcp"
    port        = "80"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "HTTPS ingress"
    direction   = "in"
    protocol    = "tcp"
    port        = "443"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "ICMP"
    direction   = "in"
    protocol    = "icmp"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }
}

# Spread nodes across physical hosts (etcd quorum survives a host failure).
resource "hcloud_placement_group" "crucible" {
  name   = "${var.name_prefix}-spread"
  type   = "spread"
  labels = var.labels
}

# Shared secret joining the RKE2 cluster (lands only in the generated,
# git-ignored ansible inventory).
resource "random_password" "rke2_token" {
  length  = 48
  special = false
}
