# One role for the gateway, assumable by Fargate tasks or the EC2 instance:
# S3 access comes from it, so no access keys exist anywhere.

data "aws_iam_policy_document" "gateway_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com", "ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "gateway" {
  name               = "${var.name}-gateway"
  assume_role_policy = data.aws_iam_policy_document.gateway_trust.json
}

data "aws_iam_policy_document" "gateway_s3" {
  statement {
    actions   = ["s3:ListBucket"]
    resources = concat([local.bucket_arn], values(local.extra_bucket_arns))
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = concat(["${local.bucket_arn}/${var.s3_prefix}*"], [for arn in values(local.extra_bucket_arns) : "${arn}/*"])
  }
}

resource "aws_iam_role_policy" "gateway_s3" {
  name   = "s3"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.gateway_s3.json
}

# The audit log outlives the task or instance that wrote it.
resource "aws_cloudwatch_log_group" "gateway" {
  name              = "/${var.name}"
  retention_in_days = var.log_retention_days
}

moved {
  from = aws_cloudwatch_log_group.ecs[0]
  to   = aws_cloudwatch_log_group.gateway
}

# Reading the secrets: the ECS execution role injects them; on EC2 the
# instance role reads them at start.
data "aws_iam_policy_document" "read_params" {
  statement {
    actions   = ["ssm:GetParameter", "ssm:GetParameters"]
    resources = local.param_arns
  }
}
