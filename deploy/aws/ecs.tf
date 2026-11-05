# compute = "ecs": Fargate tasks in two AZs behind a network load balancer.
#
# Deploys: change `image` (or users / settings) and apply. That registers a
# new task definition revision and the service rolls to it; open sessions
# on old tasks get the NLB's 120 s drain plus up to 120 s after SIGTERM.

locals {
  ecs = var.compute == "ecs" ? 1 : 0

  # About 120 MiB per concurrent large upload (measured 2026-10-31),
  # leaving ~64 MiB for the process itself.
  ecs_max_connections = coalesce(var.max_connections, max(1, floor((var.task_memory - 64) / 120)))
}

resource "aws_ecs_cluster" "this" {
  count = local.ecs
  name  = var.name
}

data "aws_iam_policy_document" "ecs_exec_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "ecs_exec" {
  count              = local.ecs
  name               = "${var.name}-ecs-exec"
  assume_role_policy = data.aws_iam_policy_document.ecs_exec_trust.json
}

resource "aws_iam_role_policy_attachment" "ecs_exec" {
  count      = local.ecs
  role       = aws_iam_role.ecs_exec[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "ecs_exec_params" {
  count  = local.ecs
  name   = "params"
  role   = aws_iam_role.ecs_exec[0].id
  policy = data.aws_iam_policy_document.read_params.json
}

resource "aws_ecs_task_definition" "this" {
  count                    = local.ecs
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  task_role_arn            = aws_iam_role.gateway.arn
  execution_role_arn       = aws_iam_role.ecs_exec[0].arn
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }
  container_definitions = jsonencode([{
    name         = "sftp-s3"
    image        = var.image
    essential    = true
    portMappings = [{ containerPort = 2222, protocol = "tcp" }]
    stopTimeout  = 120 # Fargate's maximum: open sessions get this long after SIGTERM
    environment = [
      { name = "PORT", value = "2222" },
      { name = "AWS_REGION", value = local.region },
      { name = "S3_BUCKET", value = local.bucket },
      { name = "S3_PREFIX", value = var.s3_prefix },
      { name = "SFTP_MAX_CONNECTIONS", value = tostring(local.ecs_max_connections) },
      { name = "SFTP_LOG_FORMAT", value = "json" }, # CloudWatch Logs parses the fields
      # Not read by the gateway: a new value makes a new revision, so that
      # changed users or host key reach the tasks.
      { name = "SFTP_S3_CONFIG_HASH", value = substr(local.config_hash, 0, 16) },
    ]
    secrets = [
      { name = "SFTP_USERS", valueFrom = local.users_param.arn },
      { name = "SFTP_HOST_KEY", valueFrom = local.host_key_param.arn },
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.gateway.name
        awslogs-region        = local.region
        awslogs-stream-prefix = "ecs"
      }
    }
  }])
}

# ---------- load balancer ----------

resource "aws_eip" "nlb" {
  count  = var.compute == "ecs" ? 2 : 0
  domain = "vpc"
  tags   = { Name = "${var.name}-nlb-${local.azs[count.index]}" }
}

resource "aws_security_group" "nlb" {
  count       = local.ecs
  name        = "${var.name}-nlb"
  description = "sftp-s3 load balancer"
  vpc_id      = aws_vpc.this.id
}

resource "aws_vpc_security_group_ingress_rule" "nlb_clients" {
  for_each          = var.compute == "ecs" ? toset(var.allowed_cidrs) : toset([])
  security_group_id = aws_security_group.nlb[0].id
  description       = "SFTP clients"
  ip_protocol       = "tcp"
  from_port         = var.port
  to_port           = var.port
  cidr_ipv4         = each.value
}

resource "aws_vpc_security_group_egress_rule" "nlb_to_tasks" {
  count                        = local.ecs
  security_group_id            = aws_security_group.nlb[0].id
  description                  = "forward and health-check"
  ip_protocol                  = "tcp"
  from_port                    = 2222
  to_port                      = 2222
  referenced_security_group_id = aws_security_group.gateway.id
}

resource "aws_vpc_security_group_ingress_rule" "tasks_from_nlb" {
  count                        = local.ecs
  security_group_id            = aws_security_group.gateway.id
  description                  = "from the NLB"
  ip_protocol                  = "tcp"
  from_port                    = 2222
  to_port                      = 2222
  referenced_security_group_id = aws_security_group.nlb[0].id
}

resource "aws_lb" "this" {
  count              = local.ecs
  name               = var.name
  load_balancer_type = "network"
  security_groups    = [aws_security_group.nlb[0].id]

  enable_cross_zone_load_balancing = var.cross_zone_load_balancing
  dynamic "subnet_mapping" {
    for_each = aws_subnet.public
    content {
      subnet_id     = subnet_mapping.value.id
      allocation_id = aws_eip.nlb[subnet_mapping.key].id # fixed addresses for client allowlists
    }
  }
}

resource "aws_lb_target_group" "this" {
  count                = local.ecs
  name                 = var.name
  port                 = 2222
  protocol             = "TCP"
  target_type          = "ip"
  vpc_id               = aws_vpc.this.id
  preserve_client_ip   = true # logs show the client, not the NLB
  deregistration_delay = 120  # in-flight uploads keep flowing while a task drains
  health_check {
    protocol            = "TCP"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }
}

resource "aws_lb_listener" "this" {
  count             = local.ecs
  load_balancer_arn = aws_lb.this[0].arn
  port              = var.port
  protocol          = "TCP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.this[0].arn
  }
}

resource "aws_ecs_service" "this" {
  count                              = local.ecs
  name                               = var.name
  cluster                            = aws_ecs_cluster.this[0].id
  task_definition                    = aws_ecs_task_definition.this[0].arn
  desired_count                      = var.desired_count
  launch_type                        = "FARGATE"
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.gateway.id]
    assign_public_ip = true # pulls from ECR without a NAT gateway
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.this[0].arn
    container_name   = "sftp-s3"
    container_port   = 2222
  }
  depends_on = [aws_lb_listener.this]
}
