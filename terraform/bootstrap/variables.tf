variable "region" {
  description = "AWS region for the state bucket, lock table and container registries. Must match the region of the cluster module so image pulls stay in-region."
  type        = string
  default     = "eu-central-1"
}

variable "environment" {
  description = "Environment tag applied to every resource."
  type        = string
  default     = "dev"
}

variable "project" {
  description = "Project slug used as the prefix of every resource name."
  type        = string
  default     = "togethergo"
}

variable "state_bucket_name" {
  description = "Override for the Terraform state bucket name. Leave null to use <project>-tfstate-<account-id>, which is unique without needing a random suffix."
  type        = string
  default     = null
}

variable "lock_table_name" {
  description = "Override for the DynamoDB state lock table name. Leave null to use <project>-tf-lock."
  type        = string
  default     = null
}

variable "state_retention_days" {
  description = "Days to keep noncurrent versions of state files before expiring them."
  type        = number
  default     = 90
}

variable "services" {
  description = "Components that get a container image, and therefore an ECR repository."
  type        = list(string)
  default     = ["identity", "trip", "chat", "notification", "web"]
}

variable "ecr_image_count_to_keep" {
  description = "Number of images retained per ECR repository."
  type        = number
  default     = 30
}

variable "github_repository" {
  description = "GitHub repository allowed to assume the CI role, in owner/name form. There is no default on purpose: a wrong value here would let the wrong repository into the account."
  type        = string
}

variable "github_subject_claims" {
  description = "Override for the allowed OIDC `sub` claims. Leave null to allow any ref of github_repository."
  type        = list(string)
  default     = null
}

variable "create_github_oidc_provider" {
  description = "Create the GitHub OIDC provider. Set to false if the account already has one; AWS permits only a single provider per issuer URL."
  type        = bool
  default     = true
}

variable "existing_github_oidc_provider_arn" {
  description = "ARN of an existing GitHub OIDC provider, required when create_github_oidc_provider is false."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Name of the EKS cluster the CI role may describe. Must match cluster_name in the cluster module."
  type        = string
  default     = "togethergo-dev"
}
