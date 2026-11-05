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
  description = "Addresses allowed to reach admin SSH on port 22. The deploy workflow connects from GitHub's runners, so using it means [\"0.0.0.0/0\", \"::/0\"]. Servers built from this template have password login off."
  type        = list(string)
}

variable "ssh_public_key_path" {
  description = "Public key for root on the server."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "server_name" {
  description = "Name of the server and the resources around it."
  type        = string
  default     = "wherry"
}

variable "server_type" {
  description = "Hetzner server type. cax types are arm64."
  type        = string
  default     = "cax11"
}

variable "location" {
  description = "Hetzner location."
  type        = string
  default     = "nbg1"
}

variable "image" {
  description = "Base image."
  type        = string
  default     = "ubuntu-24.04"
}

variable "release_repo" {
  description = "GitHub repository that holds the releases."
  type        = string
  default     = "maddoxrjohnson/wherry"
}
