# Partial backend configuration: the bucket and lock table names come from the
# bootstrap module's outputs and are supplied at init time, so this file never
# has to be edited per account.
#
#   terraform -chdir=terraform/bootstrap output -raw backend_config > terraform/cluster/backend.hcl
#   terraform -chdir=terraform/cluster init -backend-config=backend.hcl
#
# For `terraform validate` / `terraform fmt -check` without AWS credentials:
#
#   terraform -chdir=terraform/cluster init -backend=false
terraform {
  backend "s3" {
    key = "cluster/terraform.tfstate"
  }
}
