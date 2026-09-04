variable "name" {
  description = "Repository name, e.g. togethergo/identity."
  type        = string
}

variable "image_count_to_keep" {
  description = "Number of most recent images to retain; older ones are expired."
  type        = number
  default     = 30
}

variable "force_delete" {
  description = "Allow `terraform destroy` to delete the repository even when it still contains images. Keep this false so images survive an accidental destroy."
  type        = bool
  default     = false
}
