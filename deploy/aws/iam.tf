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
    resources = [local.bucket_arn]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${local.bucket_arn}/${var.s3_prefix}*"]
  }
}

resource "aws_iam_role_policy" "gateway_s3" {
  name   = "s3"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.gateway_s3.json
}

# Reading the secrets: the ECS execution role injects them; on EC2 the
# instance role reads them at start.
data "aws_iam_policy_document" "read_params" {
  statement {
    actions   = ["ssm:GetParameter", "ssm:GetParameters"]
    resources = local.param_arns
  }
}
