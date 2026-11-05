# compute = "ec2": one Graviton instance with a fixed address, the
# container on the SFTP port, and no sshd (administration is SSM Session
# Manager).
#
# Deploys: changing `image`, managed users or the host key replaces the
# instance (the Elastic IP moves to the new one), so expect a few minutes
# of downtime. This shape trades availability for cost; use ECS otherwise.

locals {
  ec2 = var.compute == "ec2" ? 1 : 0
}

data "aws_ssm_parameter" "al2023_arm64" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

data "aws_ec2_instance_type" "this" {
  instance_type = var.instance_type
}

locals {
  # Same sizing rule as ECS; leave ~300 MiB for the OS and docker.
  ec2_max_connections = coalesce(var.max_connections, max(1, floor((data.aws_ec2_instance_type.this.memory_size - 300) / 120)))
}

resource "aws_iam_role_policy_attachment" "ec2" {
  for_each = var.compute == "ec2" ? toset([
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
  ]) : toset([])
  role       = aws_iam_role.gateway.name
  policy_arn = each.value
}

resource "aws_iam_role_policy" "ec2_params" {
  count  = local.ec2
  name   = "params"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.read_params.json
}

resource "aws_iam_instance_profile" "ec2" {
  count = local.ec2
  name  = "${var.name}-gateway"
  role  = aws_iam_role.gateway.name
}

resource "aws_instance" "this" {
  count                  = local.ec2
  ami                    = data.aws_ssm_parameter.al2023_arm64.value
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.gateway.id]
  iam_instance_profile   = aws_iam_instance_profile.ec2[0].name
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2 # the container reaches IMDS through docker's bridge
  }
  root_block_device {
    volume_type = "gp3"
    volume_size = 8
    encrypted   = true
  }
  user_data_replace_on_change = true
  user_data = templatefile("${path.module}/ec2-user-data.sh", {
    region          = local.region
    port            = var.port
    image           = var.image
    config_hash     = local.config_hash
    param_prefix    = "/${var.name}"
    bucket          = local.bucket
    s3_prefix       = var.s3_prefix
    max_connections = local.ec2_max_connections
  })
  tags = { Name = var.name }
  lifecycle {
    ignore_changes = [ami] # a newer AMI must not replace a serving instance by itself
  }
}

resource "aws_eip" "ec2" {
  count    = local.ec2
  domain   = "vpc"
  instance = aws_instance.this[0].id
  tags     = { Name = var.name }
}
