# There is deliberately no kubernetes or helm provider in this configuration.
# Creating a cluster and deploying into it from one state makes the first apply
# unplannable (the provider needs an endpoint that does not exist yet) and the
# destroy unreliable (the provider cannot reach a cluster that is already gone).
# Everything that lives inside the cluster is applied separately with Helm and
# kubectl; see terraform/cluster/manifests and docs/TEARDOWN.md.
provider "aws" {
  region = var.region

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

data "aws_availability_zones" "available" {
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}
