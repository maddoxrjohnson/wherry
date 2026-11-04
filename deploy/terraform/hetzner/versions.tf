terraform {
  required_version = ">= 1.9"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.70"
    }
  }
}

# The token comes from HCLOUD_TOKEN so it never lands in a file or the state.
provider "hcloud" {}
