variable "bucket_name" {
  description = "Globally unique name of the S3 bucket that stores Terraform state."
  type        = string
}

variable "lock_table_name" {
  description = "Name of the DynamoDB table used for Terraform state locking."
  type        = string
}

variable "noncurrent_version_expiration_days" {
  description = "Number of days after which noncurrent state file versions are expired."
  type        = number
  default     = 90
}
