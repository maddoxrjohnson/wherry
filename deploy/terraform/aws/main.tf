locals {
  # x86 types list i386 as well, so ask about arm64 rather than take the first.
  arch = contains(data.aws_ec2_instance_type.wherry.supported_architectures, "arm64") ? "arm64" : "amd64"

  # The default VPC has no IPv6 unless someone added it.
  has_ipv6     = length(data.aws_vpc.default.ipv6_cidr_block_associations) > 0
  public_cidrs = local.has_ipv6 ? ["0.0.0.0/0", "::/0"] : ["0.0.0.0/0"]
  admin_cidrs  = [for cidr in var.admin_cidrs : cidr if local.has_ipv6 || !strcontains(cidr, ":")]

  # One rule per port and CIDR, keyed so that editing a CIDR touches only its rule.
  ingress = merge(
    { for cidr in local.admin_cidrs : "admin ssh ${cidr}" => { port = 22, cidr = cidr } },
    merge([
      for name, port in { "git ssh" = 2222, "http" = 80, "https" = 443 } : {
        for cidr in local.public_cidrs : "${name} ${cidr}" => { port = port, cidr = cidr }
      }
    ]...),
  )
}

data "aws_ec2_instance_type" "wherry" {
  instance_type = var.instance_type
}

data "aws_ami" "ubuntu" {
  owners      = ["099720109477"] # Canonical
  most_recent = true

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-${local.arch}-server-*"]
  }
}

data "aws_vpc" "default" {
  default = true
}

resource "aws_key_pair" "admin" {
  key_name   = var.server_name
  public_key = file(pathexpand(var.ssh_public_key_path))
}

resource "aws_security_group" "wherry" {
  name        = var.server_name
  description = "wherry admin ssh, git ssh, http, https"
  vpc_id      = data.aws_vpc.default.id

  tags = {
    Name = var.server_name
  }
}

resource "aws_vpc_security_group_ingress_rule" "wherry" {
  for_each = local.ingress

  security_group_id = aws_security_group.wherry.id
  description       = each.key
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = strcontains(each.value.cidr, ":") ? null : each.value.cidr
  cidr_ipv6         = strcontains(each.value.cidr, ":") ? each.value.cidr : null
}

resource "aws_vpc_security_group_egress_rule" "wherry" {
  for_each = toset(local.public_cidrs)

  security_group_id = aws_security_group.wherry.id
  description       = "all outbound"
  ip_protocol       = "-1"
  cidr_ipv4         = strcontains(each.value, ":") ? null : each.value
  cidr_ipv6         = strcontains(each.value, ":") ? each.value : null
}

resource "aws_instance" "wherry" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  key_name               = aws_key_pair.admin.key_name
  vpc_security_group_ids = [aws_security_group.wherry.id]

  # The group starts with no outbound rule, and first boot needs one. A box that
  # boots without it fails its install while Terraform reports success.
  depends_on = [aws_vpc_security_group_egress_rule.wherry]

  user_data = templatefile("${path.module}/../cloud-init.yaml.tftpl", {
    domain          = var.domain
    acme_email      = var.acme_email
    dashboard_host  = var.dashboard_host
    release_repo    = var.release_repo
    release_version = var.release_version
    arch            = local.arch
  })

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    encrypted             = true
    delete_on_termination = true
  }

  # A hop limit of 1 stops at the docker bridge, so containers and image builds
  # on this box cannot reach instance metadata.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  tags = {
    Name = var.server_name
  }

  # These only matter on first boot, and changing them would replace the server
  # and its data disk. Updates go through the running box instead.
  lifecycle {
    ignore_changes = [user_data, ami, key_name]

    precondition {
      condition     = length(local.admin_cidrs) > 0
      error_message = "admin_cidrs leaves no address that can reach SSH. IPv6 entries are skipped on a VPC without IPv6."
    }
  }
}

resource "aws_eip" "wherry" {
  domain   = "vpc"
  instance = aws_instance.wherry.id

  tags = {
    Name = var.server_name
  }
}
