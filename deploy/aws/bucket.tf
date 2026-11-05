resource "aws_s3_bucket" "this" {
  count         = var.create_bucket ? 1 : 0
  bucket        = var.bucket_name
  force_destroy = var.force_destroy_bucket
}

data "aws_s3_bucket" "existing" {
  count  = var.create_bucket ? 0 : 1
  bucket = var.bucket_name
}

data "aws_s3_bucket" "extra" {
  for_each = toset(var.extra_bucket_names)
  bucket   = each.value
}

locals {
  bucket_arn        = var.create_bucket ? aws_s3_bucket.this[0].arn : data.aws_s3_bucket.existing[0].arn
  bucket            = var.create_bucket ? aws_s3_bucket.this[0].bucket : data.aws_s3_bucket.existing[0].bucket
  extra_bucket_arns = { for name, b in data.aws_s3_bucket.extra : name => b.arn }
}

resource "aws_s3_bucket_public_access_block" "this" {
  count                   = var.create_bucket ? 1 : 0
  bucket                  = aws_s3_bucket.this[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "this" {
  count  = var.create_bucket ? 1 : 0
  bucket = aws_s3_bucket.this[0].id
  rule {
    id     = "abort-incomplete-multipart-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = var.abort_incomplete_multipart_days
    }
  }
}

# The gateway may touch the bucket only through this stack's VPC endpoint,
# which proves its uploads take the free path. Other principals (operators,
# other systems) are not affected.
data "aws_iam_policy_document" "bucket" {
  statement {
    sid     = "GatewayOnlyViaVpcEndpoint"
    effect  = "Deny"
    actions = ["s3:*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    resources = [local.bucket_arn, "${local.bucket_arn}/*"]
    condition {
      test     = "ArnEquals"
      variable = "aws:PrincipalArn"
      values   = [aws_iam_role.gateway.arn]
    }
    condition {
      test     = "StringNotEquals"
      variable = "aws:SourceVpce"
      values   = [aws_vpc_endpoint.s3.id]
    }
  }
}

data "aws_iam_policy_document" "extra_bucket" {
  for_each = local.extra_bucket_arns
  statement {
    sid     = "GatewayOnlyViaVpcEndpoint"
    effect  = "Deny"
    actions = ["s3:*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    resources = [each.value, "${each.value}/*"]
    condition {
      test     = "ArnEquals"
      variable = "aws:PrincipalArn"
      values   = [aws_iam_role.gateway.arn]
    }
    condition {
      test     = "StringNotEquals"
      variable = "aws:SourceVpce"
      values   = [aws_vpc_endpoint.s3.id]
    }
  }
}

resource "aws_s3_bucket_policy" "this" {
  count      = var.create_bucket ? 1 : 0
  bucket     = aws_s3_bucket.this[0].id
  policy     = data.aws_iam_policy_document.bucket.json
  depends_on = [aws_s3_bucket_public_access_block.this]
}
