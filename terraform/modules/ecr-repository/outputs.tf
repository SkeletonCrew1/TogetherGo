output "name" {
  description = "Repository name."
  value       = aws_ecr_repository.this.name
}

output "arn" {
  description = "Repository ARN."
  value       = aws_ecr_repository.this.arn
}

output "repository_url" {
  description = "Repository URL used as the image prefix in Helm values."
  value       = aws_ecr_repository.this.repository_url
}
