output "role_arn" {
  description = "ARN of the role GitHub Actions assumes. Use it as role-to-assume in aws-actions/configure-aws-credentials."
  value       = aws_iam_role.ci.arn
}

output "role_name" {
  description = "Name of the CI role."
  value       = aws_iam_role.ci.name
}

output "oidc_provider_arn" {
  description = "ARN of the GitHub Actions OIDC provider in use."
  value       = local.oidc_provider_arn
}
