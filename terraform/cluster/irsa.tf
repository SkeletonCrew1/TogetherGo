locals {
  alb_controller_service_account = "aws-load-balancer-controller"
  ebs_csi_service_account        = "ebs-csi-controller-sa"
  addon_namespace                = "kube-system"
}

# The controller itself is installed by Helm. Terraform only owns the AWS-side
# identity it assumes, and publishes the annotation Helm has to set.
module "alb_controller_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.44"

  role_name                              = "${var.cluster_name}-aws-load-balancer-controller"
  attach_load_balancer_controller_policy = true

  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["${local.addon_namespace}:${local.alb_controller_service_account}"]
    }
  }
}

module "ebs_csi_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts-eks"
  version = "~> 5.44"

  role_name             = "${var.cluster_name}-ebs-csi-driver"
  attach_ebs_csi_policy = true

  oidc_providers = {
    main = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["${local.addon_namespace}:${local.ebs_csi_service_account}"]
    }
  }
}
