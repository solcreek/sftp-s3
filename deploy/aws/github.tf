# A role GitHub Actions assumes through OIDC to deploy from the main branch:
# push images to the repository, then roll ECS or restart EC2.

locals {
  github = var.github_repository == null ? 0 : 1
}

data "aws_caller_identity" "current" {}

resource "aws_iam_openid_connect_provider" "github" {
  count          = local.github == 1 && !var.github_oidc_provider_exists ? 1 : 0
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

locals {
  github_oidc_arn = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/token.actions.githubusercontent.com"
}

data "aws_iam_policy_document" "github_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [local.github_oidc_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${coalesce(var.github_repository, "-")}:ref:refs/heads/main"]
    }
  }
}

resource "aws_iam_role" "github_deploy" {
  count              = local.github
  name               = "${var.name}-github-deploy"
  assume_role_policy = data.aws_iam_policy_document.github_trust.json
  depends_on         = [aws_iam_openid_connect_provider.github]
}

data "aws_iam_policy_document" "github_deploy" {
  statement {
    sid       = "EcrLogin"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid = "EcrPush"
    actions = [
      "ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage",
    ]
    resources = [aws_ecr_repository.this.arn]
  }
  dynamic "statement" {
    for_each = var.compute == "ecs" ? [1] : []
    content {
      sid       = "EcsTaskDefinitions"
      actions   = ["ecs:DescribeTaskDefinition", "ecs:RegisterTaskDefinition"]
      resources = ["*"] # these actions do not support resource-level permissions
    }
  }
  dynamic "statement" {
    for_each = var.compute == "ecs" ? [1] : []
    content {
      sid       = "EcsService"
      actions   = ["ecs:UpdateService", "ecs:DescribeServices"]
      resources = [aws_ecs_service.this[0].id]
    }
  }
  dynamic "statement" {
    for_each = var.compute == "ecs" ? [1] : []
    content {
      sid       = "PassTaskRoles"
      actions   = ["iam:PassRole"]
      resources = [aws_iam_role.gateway.arn, aws_iam_role.ecs_exec[0].arn]
    }
  }
  dynamic "statement" {
    for_each = var.compute == "ec2" ? [1] : []
    content {
      sid       = "Ec2Image"
      actions   = ["ssm:PutParameter"]
      resources = [aws_ssm_parameter.runtime["image"].arn]
    }
  }
  dynamic "statement" {
    for_each = var.compute == "ec2" ? [1] : []
    content {
      sid     = "Ec2Restart"
      actions = ["ssm:SendCommand"]
      resources = [
        aws_instance.this[0].arn,
        "arn:aws:ssm:${var.region}::document/AWS-RunShellScript",
      ]
    }
  }
  dynamic "statement" {
    for_each = var.compute == "ec2" ? [1] : []
    content {
      sid       = "Ec2RestartResult"
      actions   = ["ssm:GetCommandInvocation"]
      resources = ["*"]
    }
  }
}

resource "aws_iam_role_policy" "github_deploy" {
  count  = local.github
  name   = "deploy"
  role   = aws_iam_role.github_deploy[0].id
  policy = data.aws_iam_policy_document.github_deploy.json
}
