# Users and the host key live in SSM Parameter Store as SecureStrings; the
# gateway reads them at start (ECS through `secrets`, EC2 in its start
# script).
#
# By default this module manages both, so they are also in the OpenTofu
# state: `users` (public keys and bcrypt hashes are not secret) and a
# generated host key (which is). To keep them out of the state, set
# users = null and generate_host_key = false, then write the values with
# the AWS CLI; the module creates placeholders and never touches them again.

resource "tls_private_key" "host" {
  count     = var.generate_host_key ? 1 : 0
  algorithm = "ED25519"
}

locals {
  users_value    = var.users == null ? null : join("\n", var.users)
  host_key_value = var.generate_host_key ? tls_private_key.host[0].private_key_openssh : null

  # Changes to managed values roll the gateway (see ecs.tf and ec2.tf).
  config_hash = sha256(join("|", [coalesce(local.users_value, "external"), coalesce(local.host_key_value, "external")]))
}

resource "aws_ssm_parameter" "users" {
  count            = var.users == null ? 0 : 1
  name             = "/${var.name}/users"
  type             = "SecureString"
  value_wo         = local.users_value
  value_wo_version = parseint(substr(sha256(local.users_value), 0, 8), 16)
}

resource "aws_ssm_parameter" "users_external" {
  count = var.users == null ? 1 : 0
  name  = "/${var.name}/users"
  type  = "SecureString"
  value = "set-me"
  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_ssm_parameter" "host_key" {
  count            = var.generate_host_key ? 1 : 0
  name             = "/${var.name}/host-key"
  type             = "SecureString"
  value_wo         = local.host_key_value
  value_wo_version = parseint(substr(sha256(local.host_key_value), 0, 8), 16)
}

resource "aws_ssm_parameter" "host_key_external" {
  count = var.generate_host_key ? 0 : 1
  name  = "/${var.name}/host-key"
  type  = "SecureString"
  value = "set-me"
  lifecycle {
    ignore_changes = [value]
  }
}

locals {
  users_param    = one(concat(aws_ssm_parameter.users[*], aws_ssm_parameter.users_external[*]))
  host_key_param = one(concat(aws_ssm_parameter.host_key[*], aws_ssm_parameter.host_key_external[*]))
  param_arns     = [local.users_param.arn, local.host_key_param.arn]
}
