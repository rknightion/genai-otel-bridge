variable "name" {
  description = "Name prefix for all resources. Used as the ECS cluster name, table name suffix, and SG name."
  type        = string
  default     = "genai-otel-bridge"
}

variable "vpc_id" {
  description = "VPC ID in which the ECS service and security group are created."
  type        = string
}

variable "subnet_ids" {
  description = "Subnet IDs for the ECS service (awsvpc networking). Provide subnets in >= 2 AZs so the two tasks (active/standby) can spread across zones."
  type        = list(string)
}

variable "image" {
  description = <<-EOT
    Full container image reference (registry/repo:tag). Defaults to the public GHCR image.
    Override to point at a GHCR pull-through proxy, an ECR mirror, or a specific digest:
      image = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/genai-otel-bridge:v3.0.0"
    The image is multi-arch (amd64 + arm64 / Graviton) — Fargate selects the right variant automatically.
  EOT
  type        = string
  default     = "ghcr.io/rknightion/genai-otel-bridge:latest"
}

variable "launch_type" {
  description = <<-EOT
    ECS launch type: "FARGATE" (default, serverless) or "EC2" (bring-your-own instances).
    EC2 removes the Fargate 120 s stopTimeout cap — you can set a longer drain window.
    Fargate is recommended for most deployments: no idle-node cost, Graviton available.
  EOT
  type        = string
  default     = "FARGATE"

  validation {
    condition     = contains(["FARGATE", "EC2"], var.launch_type)
    error_message = "launch_type must be FARGATE or EC2."
  }
}

variable "cpu" {
  description = "Task CPU units (Fargate sizes: 256/512/1024/2048/4096). Default 256 matches the Helm chart requests."
  type        = number
  default     = 256
}

variable "memory" {
  description = "Task memory in MiB (Fargate sizes: 512/1024/2048/…). Default 512 matches the Helm chart limits. Also sets GOMEMLIMIT = memory * 1024 * 1024 bytes."
  type        = number
  default     = 512
}

variable "desired_count" {
  description = "Number of ECS tasks to run. 2 = active/standby (recommended). The DynamoDB lock ensures only one task polls and emits at a time regardless of count."
  type        = number
  default     = 2
}

variable "secret_arns" {
  description = <<-EOT
    Map of environment-variable name -> Secrets Manager secret ARN. Each entry is injected
    into the task as a secret (valueFrom), never as a plaintext env var. Example:
      secret_arns = {
        PORTKEY_API_KEY   = "arn:aws:secretsmanager:eu-west-1:123:secret:portkey-key-xxxx"
        LANGSMITH_API_KEY = "arn:aws:secretsmanager:eu-west-1:123:secret:langsmith-key-xxxx"
      }
    The app resolves $${ENV} and file: references at startup — these are the values those env vars hold.
  EOT
  type        = map(string)
  default     = {}
}

variable "kms_key_arn" {
  description = "ARN of a customer-managed KMS key for DynamoDB SSE. Leave null to use the AWS-owned default key (free, sufficient for most deployments)."
  type        = string
  default     = null
}

variable "config_yaml" {
  description = <<-EOT
    The rendered genai-otel-bridge YAML config injected as GENAI_OTEL_BRIDGE_CONFIG.

    Leave null (the default) to CONSUME the module's bundled, schema-generated config.example.yaml —
    the same file `make generate` produces from internal/config/config.go (drift-gated in CI), with
    ha.dynamodb.table auto-rewritten to "<var.name>-ha" to match the table this module creates. That
    default runs one Portkey source (analytics loop) with DynamoDB-backed HA; $${ENV} resolves from
    var.deployment_environment and the Grafana Cloud / Portkey secrets come from var.secret_arns.

    Override it ONLY to run a different config (e.g. extra sources/loops). When you do, set:
      ha: { coordinator: dynamodb, checkpoint: dynamodb, dynamodb: { table: <var.name>-ha } }
    All other keys follow the schema in internal/config/config.go; reference secrets as $${ENV_VAR_NAME}.
  EOT
  type        = string
  default     = null
}

variable "deployment_environment" {
  description = "Value injected as the ENV container env var, resolving the $${ENV} ref in the config (identity.deployment_environment and source_instance). Used by the bundled default config; harmless if your config_yaml has no such ref."
  type        = string
  default     = "ecs"
}

variable "aws_region" {
  description = "AWS region injected as the AWS_REGION container env var so the DynamoDB SDK client resolves the region deterministically (the bundled default config omits ha.dynamodb.region and relies on AWS_REGION). Leave null to rely on the task's ambient region resolution (Fargate metadata)."
  type        = string
  default     = null
}

variable "tags" {
  description = "Tags applied to all created resources."
  type        = map(string)
  default     = {}
}
