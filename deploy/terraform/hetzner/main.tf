locals {
  arch = startswith(var.server_type, "cax") ? "arm64" : "amd64"
}

resource "hcloud_ssh_key" "admin" {
  name       = var.server_name
  public_key = file(pathexpand(var.ssh_public_key_path))
}

resource "hcloud_firewall" "wherry" {
  name = var.server_name

  rule {
    description = "admin ssh"
    direction   = "in"
    protocol    = "tcp"
    port        = "22"
    source_ips  = var.admin_cidrs
  }

  dynamic "rule" {
    for_each = { "git ssh" = "2222", "http" = "80", "https" = "443" }
    content {
      description = rule.key
      direction   = "in"
      protocol    = "tcp"
      port        = rule.value
      source_ips  = ["0.0.0.0/0", "::/0"]
    }
  }
}

resource "hcloud_server" "wherry" {
  name         = var.server_name
  server_type  = var.server_type
  image        = var.image
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.admin.id]
  firewall_ids = [hcloud_firewall.wherry.id]

  user_data = templatefile("${path.module}/../cloud-init.yaml.tftpl", {
    domain          = var.domain
    acme_email      = var.acme_email
    dashboard_host  = var.dashboard_host
    release_repo    = var.release_repo
    release_version = var.release_version
    arch            = local.arch
  })

  # These only matter on first boot, and changing them would replace the server
  # and its data disk. Updates go through the running box instead.
  lifecycle {
    ignore_changes = [user_data, image, ssh_keys]
  }
}
