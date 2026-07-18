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

# An attached hcloud firewall filters ALL incoming traffic — including the
# private network (contrary to a common misconception). So every port RKE2 &
# co. use node-to-node (supervisor 9345, etcd 2379/2380, kubelet 10250, CNI
# vxlan 8472/udp, ceph, longhorn, NodePorts, ...) must be allowed explicitly:
# open the whole private CIDR between nodes, and keep only 22/80/443/6443 public.
resource "hcloud_firewall" "crucible" {
  name   = "${var.name_prefix}-fw"
  labels = var.labels

  rule {
    description = "All node-to-node TCP on the private network"
    direction   = "in"
    protocol    = "tcp"
    port        = "any"
    source_ips  = [var.network_cidr]
  }

  rule {
    description = "All node-to-node UDP on the private network (CNI vxlan, ...)"
    direction   = "in"
    protocol    = "udp"
    port        = "any"
    source_ips  = [var.network_cidr]
  }

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
