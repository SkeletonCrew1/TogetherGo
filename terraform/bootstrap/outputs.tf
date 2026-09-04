output "state_bucket_name" {
  description = "S3 bucket to configure as the Terraform backend of every root module."
  value       = module.tfstate.bucket_name
}

output "lock_table_name" {
  description = "DynamoDB table to configure as the Terraform state lock table."
  value       = module.tfstate.lock_table_name
}

output "ecr_repository_urls" {
  description = "ECR repository URLs keyed by service, for the image.repository values in the Helm chart."
  value       = { for name, repo in module.ecr : name => repo.repository_url }
}

output "ecr_registry" {
  description = "Registry host to docker login against."
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.region}.amazonaws.com"
}

output "ci_role_arn" {
  description = "Role ARN for aws-actions/configure-aws-credentials in GitHub Actions."
  value       = module.ci_role.role_arn
}

output "github_oidc_provider_arn" {
  description = "ARN of the GitHub Actions OIDC provider."
  value       = module.ci_role.oidc_provider_arn
}

output "region" {
  description = "Region the registries and state bucket live in."
  value       = var.region
}

output "backend_config" {
  description = "Backend block to paste into a root module, or write to a .hcl file and pass with -backend-config."
  value       = <<-EOT
    bucket         = "${module.tfstate.bucket_name}"
    key            = "cluster/terraform.tfstate"
    region         = "${var.region}"
    dynamodb_table = "${module.tfstate.lock_table_name}"
    encrypt        = true
  EOT
}
