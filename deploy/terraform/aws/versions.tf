terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.72"
    }
  }
}

# Credentials come from the standard chain, so AWS_PROFILE works and no key ever
# lands in a file or the state.
provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "wherry"
      ManagedBy = "terraform"
    }
  }
}
