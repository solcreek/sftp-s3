output "sftp_address" {
  description = "Where clients connect (port 22)."
  value       = var.compute == "ecs" ? aws_lb.this[0].dns_name : aws_eip.ec2[0].public_ip
}

output "public_ips" {
  description = "Fixed addresses to give clients that allowlist by IP."
  value       = var.compute == "ecs" ? aws_eip.nlb[*].public_ip : aws_eip.ec2[*].public_ip
}

output "ecr_repository" {
  value = aws_ecr_repository.this.repository_url
}

output "parameters" {
  description = "Set users and host-key with `aws ssm put-parameter --overwrite` (see README.md)."
  value       = { for k, p in aws_ssm_parameter.runtime : k => p.name }
}

output "github_deploy_role_arn" {
  description = "Repository variable AWS_DEPLOY_ROLE_ARN for .github/workflows/deploy.yml."
  value       = one(aws_iam_role.github_deploy[*].arn)
}

output "deploy_variables" {
  description = "Repository variables the deploy workflow reads."
  value = {
    AWS_REGION      = var.region
    DEPLOY_COMPUTE  = var.compute
    ECR_REPOSITORY  = aws_ecr_repository.this.repository_url
    ECS_CLUSTER     = one(aws_ecs_cluster.this[*].name)
    ECS_SERVICE     = one(aws_ecs_service.this[*].name)
    ECS_FAMILY      = one(aws_ecs_task_definition.this[*].family)
    EC2_INSTANCE_ID = one(aws_instance.this[*].id)
    IMAGE_PARAMETER = aws_ssm_parameter.runtime["image"].name
  }
}

output "bucket_policy_statement" {
  description = "With create_bucket = false: merge this into the existing bucket's policy by hand."
  value       = var.create_bucket ? null : data.aws_iam_policy_document.bucket.json
}
