locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.availability_zone_count)
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.0"

  name = var.cluster_name
  cidr = var.vpc_cidr
  azs  = local.azs

  # Nodes and pods live in private subnets; only the ALB is public.
  private_subnets = [for i in range(var.availability_zone_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  public_subnets  = [for i in range(var.availability_zone_count) : cidrsubnet(var.vpc_cidr, 8, i + 48)]

  enable_nat_gateway = true

  # One NAT gateway is about $33/month plus data processing. A second one buys
  # AZ-independent egress, which a dev environment does not need, and doubles
  # the single largest line item in this stack.
  single_nat_gateway = true

  enable_dns_hostnames = true
  enable_dns_support   = true

  # The AWS Load Balancer Controller discovers where to place load balancers by
  # reading these tags; without them an Ingress stays in "pending" forever.
  public_subnet_tags = {
    "kubernetes.io/role/elb"                    = "1"
    "karpenter.sh/discovery"                    = var.cluster_name
    "kubernetes.io/cluster/${var.cluster_name}" = "shared"
  }

  private_subnet_tags = {
    "kubernetes.io/role/internal-elb"           = "1"
    "karpenter.sh/discovery"                    = var.cluster_name
    "kubernetes.io/cluster/${var.cluster_name}" = "shared"
  }
}

# Every ECR image pull downloads its layers from S3. Without this endpoint that
# traffic leaves through the NAT gateway and is billed per gigabyte; a gateway
# endpoint is free and keeps it on the AWS backbone.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = module.vpc.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = module.vpc.private_route_table_ids

  tags = {
    Name = "${var.cluster_name}-s3"
  }
}
