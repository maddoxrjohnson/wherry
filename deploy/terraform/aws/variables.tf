variable "domain" {
  description = "Domain the apps live under, such as apps.example.com."
  type        = string
}

variable "acme_email" {
  description = "Contact address for the Let's Encrypt account."
  type        = string
}

variable "dashboard_host" {
  description = "Public hostname for the dashboard, such as example.com. Empty keeps it on the internal listener."
  type        = string
  default     = ""

  validation {
    condition     = var.dashboard_host == "" || can(regex("^([a-z0-9]([a-z0-9-]*[a-z0-9])?\\.)+[a-z]([a-z0-9-]*[a-z0-9])?$", var.dashboard_host))
    error_message = "Must be empty or a bare lower-case hostname, not an IP address."
  }

  validation {
    condition     = !endswith(var.dashboard_host, ".${trimsuffix(lower(var.domain), ".")}")
    error_message = "A name under the domain belongs to an app. The domain itself is fine."
  }
}

variable "release_version" {
  description = "Release tag to install on first boot, such as v0.2.0. It must already be published and ship wherry-update and wherry-backup, or first boot fails."
  type        = string
}

variable "admin_cidrs" {
  description = "Addresses allowed to reach admin SSH on port 22. The deploy workflow connects from GitHub's runners, so using it means [\"0.0.0.0/0\", \"::/0\"]. Servers built from this template have password login off. IPv6 entries are skipped when the default VPC has no IPv6."
  type        = list(string)
}

variable "ssh_public_key_path" {
  description = "Public key for the ubuntu user, who has sudo. Root login with this key is refused, so the runbook's ssh root@ steps become ssh ubuntu@ with sudo, and the forced-command deploy key has to be added to root's authorized_keys by hand."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "server_name" {
  description = "Name of the server and the resources around it."
  type        = string
  default     = "wherry"
}

variable "instance_type" {
  description = "EC2 instance type. The AMI and the release architecture follow it, so t4g.small is arm64 and t3.small is amd64."
  type        = string
  default     = "t4g.small"
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-2"
}

variable "release_repo" {
  description = "GitHub repository that holds the releases."
  type        = string
  default     = "maddoxrjohnson/wherry"
}
