# genai-otel-bridge — ECS Fargate/EC2 deployment module
#
# Provisions: DynamoDB table (lock + checkpoint), ECS cluster, ECS service (2 tasks active/standby),
# task + execution IAM roles (least-privilege), and a default-deny egress security group.
#
# Uses terraform-aws-modules where they add value:
#   dynamodb-table ~> 4.0  (on-demand, PITR, SSE, TTL) — requires AWS provider >= 5.98 (→ v6.x)
#   security-group ~> 5.0  (egress allow-list, default-deny ingress)
#   ecs/aws        ~> 7.0  (cluster module — v7 requires AWS provider >= 6.34, compatible with v6.x)
#   ecs/aws//modules/service ~> 7.0  (service submodule — v7 is still a two-module composition)
#
# Note: ECS module v5 (plan L-5) is incompatible with AWS provider v6 (which dynamodb-table ~> 4.0
# requires). We use ECS v7 which targets AWS provider v6. All service variable names are identical.
#
# Variable names verified against terraform-aws-modules GitHub sources (2026-06-28).

# ── Provider ─────────────────────────────────────────────────────────────────────────────────────

provider "aws" {
  region = var.region
}

# ── DynamoDB table (lock + checkpoint) ───────────────────────────────────────────────────────────
#
# Single table, pk (String) hash key only, on-demand billing, PITR on, SSE, TTL.
# Verified input names: name, hash_key, billing_mode, attributes, ttl_enabled,
# ttl_attribute_name, point_in_time_recovery_enabled, server_side_encryption_enabled,
# server_side_encryption_kms_key_arn, tags (all from terraform-aws-modules/dynamodb-table/aws master).

module "table" {
  source  = "terraform-aws-modules/dynamodb-table/aws"
  version = "~> 4.0"

  name         = "${var.name}-ha"
  hash_key     = "pk"
  billing_mode = "PAY_PER_REQUEST"

  attributes = [
    { name = "pk", type = "S" }
  ]

  ttl_enabled        = true
  ttl_attribute_name = "ttl"

  point_in_time_recovery_enabled = true

  server_side_encryption_enabled     = true
  server_side_encryption_kms_key_arn = var.kms_key_arn # null = AWS-owned key

  tags = var.tags
}

# ── IAM — task role (least-privilege) ────────────────────────────────────────────────────────────
#
# The task role is what the application process uses. Grants:
#   - DynamoDB: GetItem + PutItem + UpdateItem on the single HA table (lock + checkpoint).
#     DeleteItem is intentionally excluded — the coordinator never deletes the lock item
#     (it expires via TTL; see §3.3 of the design spec — no-release-needed shutdown model).
#   - SecretsManager: GetSecretValue on the caller-supplied secret ARNs (one per API key).
# The execution role (ECS agent role) is managed by the service module (create_task_exec_iam_role).

data "aws_iam_policy_document" "task" {
  statement {
    sid    = "DynamoHA"
    effect = "Allow"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
    ]
    resources = [module.table.dynamodb_table_arn]
  }

  dynamic "statement" {
    for_each = length(var.secret_arns) > 0 ? [1] : []
    content {
      sid       = "SecretsRead"
      effect    = "Allow"
      actions   = ["secretsmanager:GetSecretValue"]
      resources = values(var.secret_arns)
    }
  }
}

# ── Security group — default-deny ingress, egress allow-list ─────────────────────────────────────
#
# Mirrors the Helm NetworkPolicy egress allow-list (deploy/helm/templates/networkpolicy.yaml):
#   - DNS UDP/TCP 53 (resolver; use VPC DNS endpoint 169.254.169.253 in prod)
#   - HTTPS TCP 443 (OTLP + vendor APIs + DynamoDB public endpoint)
# In production, tighten with VPC interface endpoints for DynamoDB + Secrets Manager (no 443 egress
# to those services) and a NAT gateway for vendor API egress. The broad 0.0.0.0/0 rules here are
# a conservative default that works in every VPC topology.
#
# Ingress: the module creates no ingress rules (default-deny). The container serves health on
# 127.0.0.1 only (ECS container health check is in-task); no inbound from the VPC is needed.
#
# Verified input names for terraform-aws-modules/security-group/aws v5.x:
#   name, vpc_id, egress_with_cidr_blocks (list(map(string))), tags.
# Note: v6 (released 2026-06-03) renamed these to egress_rules — pin ~> 5.0 to keep v4/v5 API.

module "sg" {
  source  = "terraform-aws-modules/security-group/aws"
  version = "~> 5.0"

  name        = "${var.name}-egress"
  description = "genai-otel-bridge ECS task: default-deny ingress, egress allow-list (DNS + HTTPS)"
  vpc_id      = var.vpc_id

  # No ingress rules — the health check is in-task (container healthCheck CMD); no VPC inbound needed.
  ingress_with_cidr_blocks = []

  # Egress allow-list mirroring the Helm NetworkPolicy:
  egress_with_cidr_blocks = [
    {
      from_port   = 443
      to_port     = 443
      protocol    = "tcp"
      cidr_blocks = "0.0.0.0/0"
      description = "HTTPS: OTLP endpoint + vendor APIs (Portkey/LangSmith) + DynamoDB public endpoint"
    },
    {
      from_port   = 53
      to_port     = 53
      protocol    = "udp"
      cidr_blocks = "0.0.0.0/0"
      description = "DNS UDP"
    },
    {
      from_port   = 53
      to_port     = 53
      protocol    = "tcp"
      cidr_blocks = "0.0.0.0/0"
      description = "DNS TCP (fallback for large responses)"
    },
  ]

  tags = var.tags
}

# ── ECS cluster ───────────────────────────────────────────────────────────────────────────────────
#
# Cluster-only module (terraform-aws-modules/ecs/aws). The service is a separate submodule call.
# Verified input names: cluster_name, tags (from master/variables.tf).

module "cluster" {
  source  = "terraform-aws-modules/ecs/aws"
  version = "~> 7.0"

  cluster_name = var.name

  tags = var.tags
}

# ── ECS service (active/standby — 2 tasks) ───────────────────────────────────────────────────────
#
# Service submodule (terraform-aws-modules/ecs/aws//modules/service).
# v7 ECS is a TWO-module composition: cluster above + service here.
#
# Key design decisions reflected here:
#   - desired_count=2: active/standby. The DynamoDB CAS lock ensures at-most-one-advancer.
#   - awsvpc network mode (Fargate requirement; also best practice on EC2).
#   - stopTimeout=120: Fargate hard cap. Gives the leader up to 120 s to drain the in-flight
#     emit + write the final checkpoint before the task is killed. The lock is NOT released
#     on shutdown (it expires naturally — §5 of the design spec); the standby takes over
#     within ~lease_duration after expiry.
#   - healthCheck CMD: distroless has no shell; the binary's own -healthcheck mode probes
#     http://127.0.0.1:<health-port>/healthz and exits 0 (healthy) or 1 (unhealthy).
#   - enable_autoscaling=false: this is a fixed-size active/standby poller, not a capacity-scaled
#     service. Autoscaling would add tasks beyond the 2-task active/standby model for no benefit.
#   - create_tasks_iam_role=false: we supply tasks_iam_role_statements directly so the module
#     creates the role from our least-privilege policy document (avoids a separate aws_iam_role).
#
# Verified input names (from v7.5.0/modules/service/variables.tf, 2026-06-28):
#   name, cluster_arn, desired_count, launch_type, cpu, memory, container_definitions,
#   network_mode, subnet_ids, security_group_ids, tasks_iam_role_statements,
#   enable_autoscaling, deployment_minimum_healthy_percent, deployment_maximum_percent,
#   capacity_provider_strategy, tags.
#
# Container definition fields verified: image, cpu, memory, essential, environment (list of
#   {name, value}), secrets (list of {name, valueFrom}), stop_timeout, healthCheck (object with
#   command, interval, retries, startPeriod, timeout), readonlyRootFilesystem, user.

locals {
  # MEM_LIMIT in bytes = var.memory MiB. Feeds the GOMEMLIMIT env var via the existing 80%-of-limit
  # logic in the binary (same mechanism as the Helm downward-API resourceFieldRef: limits.memory).
  mem_limit_bytes = var.memory * 1024 * 1024

  # Build the secrets list for the container definition from var.secret_arns.
  # Each entry becomes a task-def secret: env var name -> Secrets Manager ARN (valueFrom).
  container_secrets = [
    for env_name, arn in var.secret_arns : {
      name      = env_name
      valueFrom = arn
    }
  ]

  # Capacity provider strategy — Fargate uses FARGATE provider; EC2 uses the cluster's default.
  # For Fargate Spot (cost savings), override capacity_provider_strategy in your root module.
  capacity_provider_strategy = var.launch_type == "FARGATE" ? {
    fargate = {
      capacity_provider = "FARGATE"
      weight            = 1
      base              = 0
    }
  } : {}
}

module "service" {
  source  = "terraform-aws-modules/ecs/aws//modules/service"
  version = "~> 7.0"

  name        = var.name
  cluster_arn = module.cluster.cluster_arn

  desired_count = var.desired_count
  cpu           = var.cpu
  memory        = var.memory

  # launch_type is left unset when using capacity_provider_strategy (they are mutually exclusive
  # in the ECS API). For EC2, set launch_type directly; for Fargate, use the capacity provider.
  launch_type = var.launch_type == "EC2" ? "EC2" : null

  capacity_provider_strategy = var.launch_type == "FARGATE" ? local.capacity_provider_strategy : {}

  # awsvpc is required for Fargate and best-practice for EC2 (one ENI per task, SG per task).
  network_mode       = "awsvpc"
  subnet_ids         = var.subnet_ids
  security_group_ids = [module.sg.security_group_id]

  # Rolling update bounds: always keep at least 1 task running (the standby) so there is never a
  # complete outage during a deployment. maximum_percent=200 allows a surge to 4 tasks briefly.
  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 200

  # Autoscaling is disabled — this is a fixed active/standby pair, not a capacity-scaled service.
  enable_autoscaling = false

  # Task IAM role — least-privilege statements (verified: tasks_iam_role_statements takes a list
  # of policy statement objects; the module creates the role + inline policy from them).
  tasks_iam_role_statements = jsondecode(data.aws_iam_policy_document.task.json)["Statement"]

  container_definitions = {
    (var.name) = {
      image     = var.image
      cpu       = var.cpu
      memory    = var.memory
      essential = true

      # MEM_LIMIT: injected as a plain env var (value known at plan time from var.memory).
      # The binary reads this at startup to set GOMEMLIMIT = 80% of the limit.
      environment = [
        {
          name  = "MEM_LIMIT"
          value = tostring(local.mem_limit_bytes)
        },
      ]

      # Secrets from Secrets Manager — injected as env vars via the ECS secrets mechanism.
      # Never appear in plaintext in the task definition or CloudWatch logs.
      secrets = local.container_secrets

      # stopTimeout=120: Fargate's maximum (AWS hard limit). Gives the leader up to 120 s to
      # complete the in-flight emit + final checkpoint Save before SIGKILL. The DynamoDB lock is
      # NOT released on shutdown (no-release model — §5 design spec); the lock expires naturally
      # and the standby acquires within ~lease_duration. On EC2 there is no 120 s cap.
      stop_timeout = 120

      # Container health check — distroless has no shell; use the binary's own -healthcheck mode.
      # The binary GETs http://127.0.0.1:8080/healthz and exits 0 (healthy) or 1 (unhealthy).
      # ECS replaces unhealthy tasks; /healthz returns 200 for both the active leader and the
      # standby (a standby is always healthy — CP-C5 parity from the Helm deployment).
      healthCheck = {
        command     = ["CMD", "/genai-otel-bridge", "-healthcheck"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 60
      }

      # Security hardening — mirrors the Helm deployment (non-root UID 65532, read-only rootfs).
      readonlyRootFilesystem = true
      user                   = "65532:65532"

      # stdout logging — ECS captures stdout/stderr; configure the log driver in the root module
      # if you need CloudWatch Logs or FireLens. Default here is the ECS awslogs driver.
      # Omitted from this module to avoid hardcoding a log group ARN — set logConfiguration in
      # your root module's container_definitions override if needed.
    }
  }

  tags = var.tags
}
