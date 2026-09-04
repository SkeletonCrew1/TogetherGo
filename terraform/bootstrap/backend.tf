# The bootstrap module creates the very bucket that stores Terraform state, so
# its first apply necessarily runs against a local state file.
#
# ONE-TIME MIGRATION TO S3, after the first successful apply:
#
#   1. terraform -chdir=terraform/bootstrap apply
#   2. terraform -chdir=terraform/bootstrap output -raw state_bucket_name
#   3. Uncomment the block below and set `bucket` to that value (and `region`
#      to var.region if you changed it from the default).
#   4. terraform -chdir=terraform/bootstrap init -migrate-state
#      Terraform asks "Do you want to copy existing state to the new backend?"
#      Answer yes.
#   5. Verify with `terraform -chdir=terraform/bootstrap plan` -> no changes.
#   6. Delete the now-stale local terraform.tfstate and terraform.tfstate.backup.
#      They are gitignored, but leaving them around invites someone to apply
#      from the wrong state later.
#
# Do not uncomment this before the first apply: the bucket does not exist yet
# and `terraform init` will fail.

# terraform {
#   backend "s3" {
#     bucket       = "togethergo-tfstate-<account-id>"
#     key          = "bootstrap/terraform.tfstate"
#     region       = "eu-central-1"
#     dynamodb_table = "togethergo-tf-lock"
#     encrypt      = true
#   }
# }
