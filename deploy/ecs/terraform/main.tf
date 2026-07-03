# genai-otel-bridge — ECS Fargate/EC2 deployment module
#
# Provisions: DynamoDB table (lock + checkpoint), ECS cluster, ECS service (2 tasks active/standby),
# task + execution IAM roles (least-privilege), and a default-deny egress security group.
#
# Uses terraform-aws-modules where they add value:
#   dynamodb-table ~> 5.0  (on-demand, PITR, SSE, no TTL) — requires AWS provider >= 5.98 (→ v6.x)
#   ecs/aws        ~> 7.0  (cluster module — v7 requires AWS provider >= 6.34, compatible with v6.x)
#   ecs/aws//modules/service ~> 7.0  (service submodule — v7 is still a two-module composition)
# Security group uses a raw aws_security_group resource with inline egress blocks (not the sg module)
# to ensure Terraform removes the AWS implicit allow-all egress and achieves true default-deny.
#
# Note: ECS module v5 (plan L-5) is incompatible with AWS provider v6 (which dynamodb-table ~> 5.0
# requires). We use ECS v7 which targets AWS provider v6. All service variable names are identical.
#
# Variable names verified against terraform-aws-modules GitHub sources (2026-06-28).

# Provider: this is a REUSABLE module — it deliberately declares NO `provider "aws"` block. The CALLER
# configures the aws provider (region/profile/assume-role); the module inherits the default provider.
# This is what lets the module be used with count/for_each and by multi-region/multi-account callers.

# ── DynamoDB table (lock + checkpoint) ───────────────────────────────────────────────────────────
#
# Single table, pk (String) hash key only, on-demand billing, PITR on, SSE, no TTL.
# Verified input names: name, hash_key, billing_mode, attributes, ttl_enabled,
# ttl_attribute_name, point_in_time_recovery_enabled, server_side_encryption_enabled,
# server_side_encryption_kms_key_arn, tags (all from terraform-aws-modules/dynamodb-table/aws master).
#
# TTL is deliberately DISABLED. The lock item must never be auto-deleted: TTL deletion would reset
# the leader fence below the durable checkpoint epoch after a long outage, leaving the coordinator
# permanently stuck. The lock is never auto-expired — it is renewed while the leader is alive and
# acquired by the standby when the lease expires naturally (parity with the Kubernetes Lease model).
# Checkpoint items carry no TTL attribute and are the durable state of record.

module "table" {
  # checkov:skip=CKV_TF_1: registry module version-pinned (~>) + .terraform.lock.hcl; commit-hash pinning is not idiomatic for terraform-aws-modules
  source  = "terraform-aws-modules/dynamodb-table/aws"
  version = "~> 5.0"

  name         = "${var.name}-ha"
  hash_key     = "pk"
  billing_mode = "PAY_PER_REQUEST"

  attributes = [
    { name = "pk", type = "S" }
  ]

  ttl_enabled = false

  point_in_time_recovery_enabled = true

  server_side_encryption_enabled     = true
  server_side_encryption_kms_key_arn = var.kms_key_arn # null = AWS-owned key

  tags = var.tags
}

# ── IAM — task role policy statements (least-privilege) ──────────────────────────────────────────
#
# The task role is what the application process uses. We build the policy as a native HCL list of
# statement objects — the exact schema the service module's `tasks_iam_role_statements` expects
# (lowercase sid/effect/actions/resources, the rest optional) — and inject it into the module-managed
# role. Grants:
#   - DynamoDB: GetItem + PutItem + UpdateItem on the single HA table (lock + checkpoint).
#     DeleteItem is intentionally excluded — the coordinator never deletes the lock item
#     (it expires naturally — §3.3 of the design spec — no-release-needed shutdown model).
#   - KMS: Decrypt + GenerateDataKey on the caller-supplied CMK (only when kms_key_arn != null).
#     Required when DynamoDB SSE uses a customer-managed key; omitted for the AWS-owned default key.
# NOTE: the task role does NOT get secretsmanager:GetSecretValue. Container-definition `secrets`
# (valueFrom Secrets Manager ARN) are fetched by the ECS agent using the EXECUTION role BEFORE the
# app process starts — not by the task role — and the app itself never calls Secrets Manager (secrets
# arrive as injected env vars). We grant that read on the execution role instead, via the service
# module's `task_exec_secret_arns` (see the module.service call below), which is least-privilege-correct.
# The execution role (ECS agent role) is managed by the service module (create_task_exec_iam_role).
#
# Built as a literal list — NOT via jsondecode(aws_iam_policy_document.json) — on purpose. The DynamoDB
# table ARN (and the Secrets Manager ARNs) are created in this SAME apply, so a policy-document data
# source that references them is deferred to apply: its `.json` is unknown at plan, and a jsondecode of
# an unknown string yields a wholly-unknown value. That unknown propagates into the service module's
# `count = local.create_tasks_iam_role && (var.tasks_iam_role_statements != null ...) ? 1 : 0`, which
# OpenTofu rejects ("count ... cannot be determined until apply"). A literal list keeps the list itself
# known at plan (known length, non-null) — only the nested resource ARNs stay unknown, which is fine —
# so the module's count resolves and the deploy converges in a single apply.

locals {
  task_iam_statements = concat(
    [
      {
        sid       = "DynamoHA"
        effect    = "Allow"
        actions   = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem"]
        resources = [module.table.dynamodb_table_arn]
      },
    ],
    var.kms_key_arn != null ? [
      {
        sid       = "KMSDecrypt"
        effect    = "Allow"
        actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
        resources = [var.kms_key_arn]
      },
    ] : [],
  )
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
# Ingress: no ingress blocks — default-deny. The health check is in-task (container healthCheck CMD);
# no inbound from the VPC is needed.
#
# Why raw aws_security_group (not terraform-aws-modules/security-group/aws):
#   The terraform-aws-modules/security-group/aws ~> 5.0 module manages rules via separate
#   aws_security_group_rule resources and does NOT set the inline egress block, so AWS's implicit
#   "allow all egress" rule (0.0.0.0/0 -1) is left in place. True default-deny egress requires
#   inline egress {} blocks on the aws_security_group resource itself — Terraform removes the
#   implicit AWS allow-all only when it owns the inline rules. A raw resource with explicit inline
#   egress blocks is the correct approach; the module cannot achieve this.

resource "aws_security_group" "this" {
  # checkov:skip=CKV2_AWS_5: attached to the ECS service via module.service security_group_ids; checkov can't see it without --download-external-modules
  name        = "${var.name}-egress"
  description = "genai-otel-bridge ECS task: default-deny ingress, egress allow-list (DNS + HTTPS)"
  vpc_id      = var.vpc_id

  # Egress allow-list mirroring the Helm NetworkPolicy.
  # Inline blocks here cause Terraform to own the egress rules, removing AWS's implicit allow-all.
  egress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    # [issue #128] "vendor APIs" here covers BOTH the Portkey control-plane API (api.portkey.ai) AND
    # the DIFFERENT host the logs_export loop downloads export objects from (a signed S3 URL — see
    # config sources[].signed_url_allow_hosts, internal/source/portkey/CLAUDE.md, docs/DESIGN.md §4.7).
    # This rule is already 0.0.0.0/0 so both are reachable today; if you ever narrow it to specific
    # CIDRs, you must include the S3 signed-URL host's range or logs_export downloads stall silently.
    description = "HTTPS: OTLP endpoint + vendor APIs (Portkey control-plane API + its S3 signed-URL export/download host + LangSmith) + DynamoDB public endpoint"
  }

  egress {
    from_port   = 53
    to_port     = 53
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
    description = "DNS UDP"
  }

  egress {
    from_port   = 53
    to_port     = 53
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    description = "DNS TCP (fallback for large responses)"
  }

  # No ingress blocks — default-deny ingress. The container health check is in-task;
  # no inbound from the VPC is needed.

  tags = merge(var.tags, { Name = "${var.name}-egress" })
}

# ── ECS cluster ───────────────────────────────────────────────────────────────────────────────────
#
# Cluster-only module (terraform-aws-modules/ecs/aws). The service is a separate submodule call.
# Verified input names: cluster_name, tags (from master/variables.tf).

module "cluster" {
  # checkov:skip=CKV_TF_1: registry module version-pinned (~>) + lock file; commit-hash pinning is not idiomatic for terraform-aws-modules
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
#   - tasks_iam_role_statements: the module creates the task IAM role (create_tasks_iam_role=true,
#     the module default) and injects our least-privilege policy document into the module-managed
#     role. This avoids a separate aws_iam_role resource.
#
# Verified input names (from v7.5.0/modules/service/variables.tf, 2026-06-28):
#   name, cluster_arn, desired_count, launch_type, cpu, memory, container_definitions,
#   network_mode, subnet_ids, security_group_ids, tasks_iam_role_statements,
#   enable_autoscaling, deployment_minimum_healthy_percent, deployment_maximum_percent,
#   capacity_provider_strategy, tags.
#
# Container definition fields verified (all camelCase per sub-module schema): image, cpu, memory,
#   essential, environment (list of {name, value}), secrets (list of {name, valueFrom}),
#   stopTimeout, healthCheck (object with command, interval, retries, startPeriod, timeout),
#   readonlyRootFilesystem, user.

locals {
  # Effective config: the caller's config_yaml, or — when null — the module's bundled, schema-generated
  # config.example.yaml (the SAME file `make generate` produces, drift-gated in CI). The generated file
  # carries the generic table name "genai-otel-bridge-ha" (it's name-agnostic); rewrite it to the table
  # THIS module actually creates ("<var.name>-ha"). The token is unique to the table line (lock_name is
  # "…-leader", service_namespace is "genai-otel-bridge" with no "-ha"), so the replace is unambiguous.
  effective_config = var.config_yaml != null ? var.config_yaml : replace(
    file("${path.module}/config.example.yaml"),
    "table: genai-otel-bridge-ha",
    "table: ${var.name}-ha",
  )

  # MEM_LIMIT in bytes = var.memory MiB. Passed to the binary's -container-mem-bytes flag (see the
  # container `command` below), which sets GOMEMLIMIT = 0.9 × this value (selfobs.SetMemoryLimit) —
  # the same soft-limit mechanism as the Helm downward-API resourceFieldRef: limits.memory path, which
  # also feeds -container-mem-bytes. (The env var alone is inert: the Go runtime only reads a var named
  # GOMEMLIMIT, and the binary reads the limit from the flag, not from MEM_LIMIT.)
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
  # checkov:skip=CKV_TF_1: registry module version-pinned (~>) + lock file; commit-hash pinning is not idiomatic for terraform-aws-modules
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
  security_group_ids = [aws_security_group.this.id]

  # Rolling update bounds: always keep at least 1 task running (the standby) so there is never a
  # complete outage during a deployment. maximum_percent=200 allows a surge to 4 tasks briefly.
  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 200

  # Autoscaling is disabled — this is a fixed active/standby pair, not a capacity-scaled service.
  enable_autoscaling = false

  # Task IAM role — the module creates the role by default (create_tasks_iam_role=true, the
  # module default). tasks_iam_role_statements injects our least-privilege policy document into
  # the module-managed role (verified: the variable accepts a list of policy statement objects).
  tasks_iam_role_statements = local.task_iam_statements

  # Execution role secret access — the ECS AGENT (execution role, not the task role) fetches the
  # container-definition `secrets` (valueFrom Secrets Manager ARN) at task launch, before the app
  # starts. The module adds a scoped secretsmanager:GetSecretValue statement to the module-created
  # execution role for exactly these ARNs. WITHOUT this, every task launch fails with
  # ResourceInitializationError / AccessDeniedException and the service never reaches RUNNING.
  # Secrets encrypted with the AWS-managed aws/secretsmanager key need no extra grant (the key policy
  # covers it); secrets encrypted with a CUSTOMER-managed KMS key additionally need kms:Decrypt on that
  # key on the execution role — pass it via task_exec_iam_statements in your root module (the secrets
  # CMK may differ from var.kms_key_arn, which is the DynamoDB SSE key).
  task_exec_secret_arns = values(var.secret_arns)

  container_definitions = {
    (var.name) = {
      image     = var.image
      cpu       = var.cpu
      memory    = var.memory
      essential = true

      # Container command (docker CMD, appended to the image ENTRYPOINT ["/genai-otel-bridge"]).
      # -container-mem-bytes is what actually sets GOMEMLIMIT (= 0.9 × the value) — the Go runtime only
      # honours a var literally named GOMEMLIMIT, and the binary reads the limit from this flag, so the
      # MEM_LIMIT env var alone would be a no-op. This mirrors the Helm deployment, which passes
      # -container-mem-bytes=$(MEM_LIMIT). No -config flag: config is delivered via the
      # GENAI_OTEL_BRIDGE_CONFIG env var (ECS/Fargate has no file mount), and identity resolves from the
      # ECS task-metadata endpoint — so only the memory flag is appended here.
      command = ["-container-mem-bytes", tostring(local.mem_limit_bytes)]

      # Environment variables injected at plan time (static values).
      # GENAI_OTEL_BRIDGE_CONFIG: the full YAML config (non-secret structure; secret values arrive
      #   via the Secrets Manager `secrets` injection below as ${ENV_VAR_NAME} placeholders).
      # MEM_LIMIT: informational only — the same byte value is passed to -container-mem-bytes above
      #   (which is what actually sets GOMEMLIMIT = 0.9 × it); kept for parity/visibility with Helm.
      # ENV: resolves ${ENV} in the config (identity.deployment_environment, source_instance).
      # GENAI_OTEL_BRIDGE_REPLICAS: mirrors the Helm chart's deployment.yaml injection — gives the
      #   binary's all-leader double-emit guard (main.go, ha.coordinator=none + replicas>1 ⇒ fatal at
      #   startup) the same defence-in-depth on ECS. The DynamoDB CAS lock is the real anti-double-emit
      #   guarantee when ha.coordinator=dynamodb (the module default); this only catches an operator who
      #   deliberately mis-sets coordinator=none with desired_count>1 (issue #125).
      # AWS_REGION (only when var.aws_region is set): lets the DynamoDB SDK resolve the region when the
      #   config omits ha.dynamodb.region (the bundled default does).
      environment = concat([
        {
          name  = "GENAI_OTEL_BRIDGE_CONFIG"
          value = local.effective_config
        },
        {
          name  = "MEM_LIMIT"
          value = tostring(local.mem_limit_bytes)
        },
        {
          name  = "ENV"
          value = var.deployment_environment
        },
        {
          name  = "GENAI_OTEL_BRIDGE_REPLICAS"
          value = tostring(var.desired_count)
        },
        ], var.aws_region != null ? [{
          name  = "AWS_REGION"
          value = var.aws_region
      }] : [])

      # Secrets from Secrets Manager — injected as env vars via the ECS secrets mechanism.
      # Never appear in plaintext in the task definition or CloudWatch logs.
      secrets = local.container_secrets

      # stopTimeout=120: Fargate's maximum (AWS hard limit). On SIGTERM the root context is cancelled,
      # which ABORTS any in-flight emit and checkpoint Save (deliberately — the design does NOT drain or
      # persist a final watermark on the way out; a partial window is simply re-pulled by the next
      # leader from the last committed watermark). stopTimeout just bounds the wait before SIGKILL. The
      # DynamoDB lock is NOT released on shutdown (no-release model — §5 design spec); it expires
      # naturally and the standby acquires within ~lease_duration. On EC2 there is no 120 s cap.
      # Key is camelCase (stopTimeout) as required by the container-definition sub-module schema.
      stopTimeout = 120

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
