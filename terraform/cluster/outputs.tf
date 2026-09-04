output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_arn" {
  description = "EKS cluster ARN."
  value       = module.eks.cluster_arn
}

output "cluster_version" {
  description = "Running control plane version."
  value       = module.eks.cluster_version
}

output "region" {
  description = "Region the cluster runs in."
  value       = var.region
}

output "vpc_id" {
  description = "VPC the cluster runs in."
  value       = module.vpc.vpc_id
}

output "private_subnet_ids" {
  description = "Private subnets holding the nodes."
  value       = module.vpc.private_subnets
}

output "public_subnet_ids" {
  description = "Public subnets the ALB is placed in."
  value       = module.vpc.public_subnets
}

output "certificate_arn" {
  description = "ARN of the validated ACM certificate, or null when create_certificate is false. Use it as the alb.ingress.kubernetes.io/certificate-arn annotation."
  value       = var.create_certificate ? aws_acm_certificate_validation.this[0].certificate_arn : null
}

output "alb_controller_role_arn" {
  description = "IRSA role for the AWS Load Balancer Controller."
  value       = module.alb_controller_irsa.iam_role_arn
}

output "ebs_csi_role_arn" {
  description = "IRSA role for the EBS CSI driver addon."
  value       = module.ebs_csi_irsa.iam_role_arn
}

output "service_account_annotations" {
  description = "Annotations Helm must put on each service account. The chart values are serviceAccount.annotations for the controller chart."
  value = {
    "${local.addon_namespace}/${local.alb_controller_service_account}" = {
      "eks.amazonaws.com/role-arn" = module.alb_controller_irsa.iam_role_arn
    }
    "${local.addon_namespace}/${local.ebs_csi_service_account}" = {
      "eks.amazonaws.com/role-arn" = module.ebs_csi_irsa.iam_role_arn
    }
  }
}

output "update_kubeconfig_command" {
  description = "Command that points the local kubectl at this cluster."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

output "alb_controller_helm_command" {
  description = "Helm command that installs the AWS Load Balancer Controller against the IRSA role created here."
  value       = <<-EOT
    helm repo add eks https://aws.github.io/eks-charts && helm repo update
    helm upgrade --install aws-load-balancer-controller eks/aws-load-balancer-controller \
      --namespace ${local.addon_namespace} \
      --set clusterName=${module.eks.cluster_name} \
      --set region=${var.region} \
      --set vpcId=${module.vpc.vpc_id} \
      --set serviceAccount.create=true \
      --set serviceAccount.name=${local.alb_controller_service_account} \
      --set-string serviceAccount.annotations."eks\\.amazonaws\\.com/role-arn"=${module.alb_controller_irsa.iam_role_arn}
  EOT
}
