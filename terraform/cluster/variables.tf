variable "region" {
  description = "AWS region. Must match the bootstrap module so nodes pull images from an in-region registry."
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

variable "cluster_name" {
  description = "EKS cluster name. Must match cluster_name in the bootstrap module so the CI role can describe it."
  type        = string
  default     = "togethergo-dev"
}

variable "kubernetes_version" {
  description = "EKS control plane version."
  type        = string
  default     = "1.31"
}

# --- Networking -------------------------------------------------------------

variable "vpc_cidr" {
  description = "CIDR block of the VPC. /16 leaves plenty of room for the VPC CNI, which allocates pod IPs from the subnets."
  type        = string
  default     = "10.42.0.0/16"
}

variable "availability_zone_count" {
  description = "Number of availability zones. Two is the minimum EKS accepts."
  type        = number
  default     = 2

  validation {
    condition     = var.availability_zone_count >= 2
    error_message = "EKS requires subnets in at least two availability zones."
  }
}

variable "cluster_endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public Kubernetes API endpoint. The default is open to the internet so a first apply works from anywhere. OPERATOR: narrow this to your office/VPN egress IP as soon as the cluster is up, e.g. [\"203.0.113.4/32\"]."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

# --- Node group -------------------------------------------------------------

variable "node_instance_types" {
  description = "Instance types for the managed node group. t4g is Graviton (ARM64); service images must be built for linux/arm64."
  type        = list(string)
  default     = ["t4g.medium"]
}

variable "node_ami_type" {
  description = "EKS managed node group AMI type. Must match the architecture of node_instance_types."
  type        = string
  default     = "AL2023_ARM_64_STANDARD"
}

variable "node_group_min_size" {
  description = "Minimum number of nodes."
  type        = number
  default     = 2
}

variable "node_group_max_size" {
  description = "Maximum number of nodes."
  type        = number
  default     = 4
}

variable "node_group_desired_size" {
  description = "Desired number of nodes at creation. The autoscaler, if installed later, owns this value afterwards."
  type        = number
  default     = 2
}

variable "node_disk_size_gb" {
  description = "Root volume size per node. Five service images plus Postgres, Redis and RabbitMQ do not fit comfortably in the 20 GB default."
  type        = number
  default     = 40
}

variable "cluster_enabled_log_types" {
  description = "Control plane log types shipped to CloudWatch. Each enabled type costs ingestion; api and audit are the two worth paying for."
  type        = list(string)
  default     = ["api", "audit"]
}

variable "cluster_log_retention_days" {
  description = "Retention of the control plane CloudWatch log group."
  type        = number
  default     = 7
}

# --- TLS --------------------------------------------------------------------

variable "create_certificate" {
  description = "Issue an ACM certificate and its Route 53 validation records. Set to false to apply the module without owning a domain; the ALB then serves plain HTTP."
  type        = bool
  default     = false
}

variable "domain_name" {
  description = "Apex domain for the certificate, e.g. togethergo.example.com. Required when create_certificate is true."
  type        = string
  default     = null

  validation {
    condition     = !var.create_certificate || var.domain_name != null
    error_message = "domain_name must be set when create_certificate is true."
  }
}

variable "subject_alternative_names" {
  description = "Extra names on the certificate. A wildcard such as *.example.com validates through the same DNS record as its parent domain, so listing both is safe."
  type        = list(string)
  default     = []
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone id that serves domain_name. Required when create_certificate is true."
  type        = string
  default     = null

  validation {
    condition     = !var.create_certificate || var.hosted_zone_id != null
    error_message = "hosted_zone_id must be set when create_certificate is true."
  }
}
