resource "aws_ecr_repository" "this" {
  name = var.name

  # Immutable tags mean a deployed tag can never silently change underneath a
  # running cluster. CI must push a new tag (the commit SHA) for every build.
  image_tag_mutability = "IMMUTABLE"

  force_delete = var.force_delete

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }
}

# Untagged and superseded images are pure cost. Keep the last N of each
# repository, which covers rollback to any recent release.
resource "aws_ecr_lifecycle_policy" "this" {
  repository = aws_ecr_repository.this.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 7 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = {
          type = "expire"
        }
      },
      {
        rulePriority = 2
        description  = "Keep only the ${var.image_count_to_keep} most recent images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = var.image_count_to_keep
        }
        action = {
          type = "expire"
        }
      },
    ]
  })
}
