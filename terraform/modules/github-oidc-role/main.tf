locals {
  oidc_provider_arn = var.create_oidc_provider ? aws_iam_openid_connect_provider.github[0].arn : var.existing_oidc_provider_arn

  subject_claims = coalesce(var.github_subject_claims, ["repo:${var.github_repository}:*"])
}

# GitHub Actions federates into AWS with a short-lived OIDC token, so there are
# no long-lived access keys stored as repository secrets anywhere.
resource "aws_iam_openid_connect_provider" "github" {
  count = var.create_oidc_provider ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  thumbprint_list = [
    # Root CA thumbprints published by GitHub. AWS verifies the issuer against
    # its own trust store as well, but the API still stores what is set here.
    "6938fd4d98bab03faadb97b34396831e3780aea1",
    "1c58a3a8518e8759bf075b76b750d4f2df264fcd",
  ]
}

data "aws_iam_policy_document" "assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.oidc_provider_arn]
    }

    # Without the aud condition the role would be assumable by any GitHub
    # repository in the world that targets this account.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = local.subject_claims
    }
  }
}

resource "aws_iam_role" "ci" {
  name               = var.role_name
  description        = "Assumed by GitHub Actions in ${var.github_repository} to push images and read cluster metadata."
  assume_role_policy = data.aws_iam_policy_document.assume_role.json

  # Slightly longer than the longest expected build, well short of the 12h max.
  max_session_duration = 3600
}

data "aws_iam_policy_document" "ci" {
  # GetAuthorizationToken is account-wide by definition: it returns the registry
  # login, not access to any particular repository.
  statement {
    sid       = "EcrLogin"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid    = "EcrPush"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:CompleteLayerUpload",
      "ecr:DescribeImages",
      "ecr:DescribeRepositories",
      "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload",
      "ecr:ListImages",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
    resources = var.ecr_repository_arns
  }

  # Enough to run `aws eks update-kubeconfig` before `helm upgrade`. Cluster
  # authorisation itself is granted separately by an EKS access entry.
  statement {
    sid       = "EksDescribe"
    effect    = "Allow"
    actions   = ["eks:DescribeCluster"]
    resources = [var.eks_cluster_arn_pattern]
  }

  statement {
    sid       = "EksList"
    effect    = "Allow"
    actions   = ["eks:ListClusters"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ci" {
  name   = "${var.role_name}-policy"
  role   = aws_iam_role.ci.id
  policy = data.aws_iam_policy_document.ci.json
}
