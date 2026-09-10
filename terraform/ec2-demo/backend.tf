terraform {
  backend "s3" {
    bucket       = "togethergo-state-file-1337"
    key          = "ec2-demo/terraform.tfstate"
    region       = "eu-north-1"
    encrypt      = true
    use_lockfile = true
  }
}