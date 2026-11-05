# Calls the module in deploy/aws. Outside this repository, use
#   source = "github.com/solcreek/sftp-s3//deploy/aws?ref=v0.2.1"
# Copy terraform.tfvars.example to terraform.tfvars and edit it.

terraform {
  required_version = ">= 1.8"
  # For a team, keep the state in a shared backend, e.g.:
  # backend "s3" { bucket = "my-tfstate" key = "sftp-s3.tfstate" region = "..." use_lockfile = true }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { project = "sftp-s3" }
  }
}

module "sftp" {
  source = "../../deploy/aws"

  compute       = var.compute
  bucket_name   = var.bucket_name
  create_bucket = var.create_bucket
  allowed_cidrs = var.allowed_cidrs
  users         = var.users
  image         = var.image
}

variable "region" { type = string }
variable "compute" {
  type    = string
  default = "ecs"
}
variable "bucket_name" { type = string }
variable "create_bucket" {
  type    = bool
  default = true
}
variable "allowed_cidrs" { type = list(string) }
variable "users" { type = list(string) }
variable "image" {
  type    = string
  default = "ghcr.io/solcreek/sftp-s3:0.2"
}

output "sftp_address" { value = module.sftp.sftp_address }
output "public_ips" { value = module.sftp.public_ips }
output "host_key_fingerprint" { value = module.sftp.host_key_fingerprint }
output "known_hosts" { value = module.sftp.known_hosts }
