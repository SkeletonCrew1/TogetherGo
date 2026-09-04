provider "aws" {
  region = var.region

  # Applied to every resource that supports tagging, so no resource block needs
  # to repeat them.
  default_tags {
    tags = {
      Project     = "togethergo"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}
