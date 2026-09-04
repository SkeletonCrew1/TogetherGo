variable "role_name" {
  description = "Name of the IAM role GitHub Actions assumes."
  type        = string
}

variable "create_oidc_provider" {
  description = "Create the GitHub Actions OIDC provider. Set to false when the account already has one (an account may only have a single provider per issuer URL)."
  type        = bool
  default     = true
}

variable "existing_oidc_provider_arn" {
  description = "ARN of a pre-existing GitHub Actions OIDC provider. Required when create_oidc_provider is false."
  type        = string
  default     = null
}

variable "github_repository" {
  description = "Repository allowed to assume the role, in owner/name form. The trust policy is scoped to this repository only."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be in owner/name form, e.g. my-org/TogetherGo."
  }
}

variable "github_subject_claims" {
  description = "Explicit `sub` claim patterns allowed to assume the role. Defaults to any ref of the configured repository. Narrow this to e.g. repo:owner/name:ref:refs/heads/main to restrict deploys to one branch."
  type        = list(string)
  default     = null
}

variable "ecr_repository_arns" {
  description = "ARNs of the ECR repositories CI may push to."
  type        = list(string)
}

variable "eks_cluster_arn_pattern" {
  description = "ARN pattern of the EKS clusters CI may describe, used to fetch kubeconfig before running helm."
  type        = string
}
