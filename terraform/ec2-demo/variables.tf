variable "region" {
  description = "AWS region in which to create the temporary deployment."
  type        = string
  default     = "eu-central-1"
}

variable "project" {
  description = "Name prefix for resources."
  type        = string
  default     = "togethergo"
}

variable "environment" {
  description = "Environment tag and name suffix."
  type        = string
  default     = "demo"
}

variable "instance_type" {
  description = "EC2 instance type. Eight GiB is recommended while images build locally."
  type        = string
  default     = "t3.micro"
}

variable "root_volume_size_gb" {
  description = "Size of the encrypted gp3 root volume."
  type        = number
  default     = 40

  validation {
    condition     = var.root_volume_size_gb >= 30
    error_message = "root_volume_size_gb must be at least 30."
  }
}

variable "ssh_key_name" {
  description = "Optional name of an existing EC2 key pair. Leave null to use Session Manager only."
  type        = string
  default     = null
}

variable "ssh_ingress_cidr" {
  description = "Optional single CIDR allowed to use SSH, normally your public IP with /32. Requires ssh_key_name."
  type        = string
  default     = null

  validation {
    condition     = var.ssh_ingress_cidr == null || var.ssh_key_name != null
    error_message = "ssh_key_name must be set when ssh_ingress_cidr is set."
  }
}

variable "domain_name" {
  description = "Optional application FQDN, for example togethergo.example.com."
  type        = string
  default     = null
}

variable "route53_zone_id" {
  description = "Optional existing public Route 53 hosted zone ID. Leave null when DNS is hosted elsewhere."
  type        = string
  default     = null

  validation {
    condition     = var.route53_zone_id == null || var.domain_name != null
    error_message = "domain_name must be set when route53_zone_id is set."
  }
}
