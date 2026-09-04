locals {
  state_bucket_name = coalesce(
    var.state_bucket_name,
    "${var.project}-tfstate-${data.aws_caller_identity.current.account_id}",
  )

  lock_table_name = coalesce(var.lock_table_name, "${var.project}-tf-lock")
}

module "tfstate" {
  source = "../modules/tfstate-backend"

  bucket_name                        = local.state_bucket_name
  lock_table_name                    = local.lock_table_name
  noncurrent_version_expiration_days = var.state_retention_days
}

module "ecr" {
  source   = "../modules/ecr-repository"
  for_each = toset(var.services)

  name                = "${var.project}/${each.value}"
  image_count_to_keep = var.ecr_image_count_to_keep
}

module "ci_role" {
  source = "../modules/github-oidc-role"

  role_name                  = "${var.project}-github-actions"
  create_oidc_provider       = var.create_github_oidc_provider
  existing_oidc_provider_arn = var.existing_github_oidc_provider_arn
  github_repository          = var.github_repository
  github_subject_claims      = var.github_subject_claims

  ecr_repository_arns = [for repo in module.ecr : repo.arn]

  # The cluster is created and destroyed by the other root module, so its ARN is
  # not knowable here. Scope by name pattern instead of granting eks:* on "*".
  eks_cluster_arn_pattern = "arn:${data.aws_partition.current.partition}:eks:${var.region}:${data.aws_caller_identity.current.account_id}:cluster/${var.cluster_name}"
}
