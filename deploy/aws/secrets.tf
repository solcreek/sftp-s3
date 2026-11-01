# Runtime settings in SSM Parameter Store. Secrets stay out of the
# OpenTofu state: the stack creates placeholders and never reads the values
# back, so set the real ones with the CLI (see README.md). The gateway
# refuses to start while SFTP_USERS is still the placeholder.
#
# The image parameter is what EC2 runs; CI updates it on every deploy.

locals {
  runtime_params = {
    users    = { type = "SecureString", value = "set-me" }
    host-key = { type = "SecureString", value = "set-me" }
    image    = { type = "String", value = "${aws_ecr_repository.this.repository_url}:${var.image_tag}" }
  }
}

resource "aws_ssm_parameter" "runtime" {
  for_each = local.runtime_params
  name     = "/${var.name}/${each.key}"
  type     = each.value.type
  value    = each.value.value
  lifecycle {
    ignore_changes = [value]
  }
}
