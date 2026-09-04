module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.31"

  cluster_name    = var.cluster_name
  cluster_version = var.kubernetes_version

  # Public endpoint so an operator can reach the API without a bastion. The CIDR
  # list is what actually restricts it; see the variable description.
  cluster_endpoint_public_access       = true
  cluster_endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs
  cluster_endpoint_private_access      = true

  # IRSA is how the load balancer controller and the EBS CSI driver get AWS
  # permissions without node-wide credentials.
  enable_irsa = true

  authentication_mode                      = "API_AND_CONFIG_MAP"
  enable_cluster_creator_admin_permissions = true

  cluster_enabled_log_types              = var.cluster_enabled_log_types
  cloudwatch_log_group_retention_in_days = var.cluster_log_retention_days

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  cluster_addons = {
    # before_compute installs the CNI before nodes join, so the first node does
    # not come up with the default (and immediately replaced) configuration.
    vpc-cni = {
      before_compute = true
    }
    coredns    = {}
    kube-proxy = {}
    # aws-ebs-csi-driver is deliberately not here: it needs an IRSA role whose
    # trust policy depends on this module's OIDC provider, which would be a
    # dependency cycle. It is a separate aws_eks_addon resource below.
  }

  eks_managed_node_groups = {
    default = {
      ami_type       = var.node_ami_type
      instance_types = var.node_instance_types
      capacity_type  = "ON_DEMAND"

      min_size     = var.node_group_min_size
      max_size     = var.node_group_max_size
      desired_size = var.node_group_desired_size

      block_device_mappings = {
        root = {
          device_name = "/dev/xvda"
          ebs = {
            volume_size           = var.node_disk_size_gb
            volume_type           = "gp3"
            encrypted             = true
            delete_on_termination = true
          }
        }
      }

      # Lets an operator open a shell on a node with `aws ssm start-session`
      # instead of exposing SSH.
      iam_role_additional_policies = {
        AmazonSSMManagedInstanceCore = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
      }

      labels = {
        workload = "general"
      }
    }
  }
}

# Installed after the node group exists, because the driver's controller pods
# need somewhere to be scheduled before the addon reports ACTIVE.
resource "aws_eks_addon" "ebs_csi" {
  cluster_name             = module.eks.cluster_name
  addon_name               = "aws-ebs-csi-driver"
  service_account_role_arn = module.ebs_csi_irsa.iam_role_arn

  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  depends_on = [module.eks]
}
