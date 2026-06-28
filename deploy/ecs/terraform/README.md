# genai-otel-bridge — ECS Terraform module

Reusable Terraform module that deploys `genai-otel-bridge` on AWS ECS (Fargate default, EC2 optional)
with full active/standby leader-elected HA backed by DynamoDB — the same single-emit, gap-free model
as the Kubernetes/Helm deployment.

Built on [terraform-aws-modules](https://registry.terraform.io/namespaces/terraform-aws-modules):
- `terraform-aws-modules/dynamodb-table/aws ~> 4.0` — lock + checkpoint table
- `aws_security_group` (raw resource) — default-deny egress security group
- `terraform-aws-modules/ecs/aws ~> 7.0` — ECS cluster
- `terraform-aws-modules/ecs/aws//modules/service ~> 7.0` — ECS service (2 tasks active/standby)

One multi-arch image (`amd64` + `arm64`/Graviton) runs on both ECS and Kubernetes — no separate build.

---

## Quick-start usage

```hcl
locals {
  # The module always creates the table as "${var.name}-ha".
  # Use this local (not module.genai_otel_bridge.table_name) inside the module block itself
  # to avoid a self-referential dependency cycle.
  bridge_name       = "genai-otel-bridge"
  bridge_table_name = "${local.bridge_name}-ha"
  bridge_region     = "eu-west-1"
}

module "genai_otel_bridge" {
  source = "github.com/rknightion/genai-otel-bridge//deploy/ecs/terraform"

  name       = local.bridge_name
  region     = local.bridge_region
  vpc_id     = "vpc-0abc123"
  subnet_ids = ["subnet-0aaa", "subnet-0bbb"]  # >= 2 AZs for active/standby spread

  # config_yaml is the rendered app config injected as GENAI_OTEL_BRIDGE_CONFIG env var.
  # The ha.* block must point at the table this module creates (always "${name}-ha").
  # Secret values go in secret_arns (never inline here); reference them as ${MY_API_KEY} in config.
  # NOTE on $: ${...} is Terraform interpolation (used here to inject the table/region locals);
  # $${...} is escaped so the LITERAL ${VAR} reaches the binary, which resolves it from the
  # Secrets-Manager-injected env vars at load time. The structure below mirrors the chart's config:
  # block (deploy/helm/values.yaml) — see internal/config/config.go for the full schema.
  config_yaml = <<-EOT
    ha:
      coordinator: dynamodb
      checkpoint: dynamodb
      dynamodb:
        table: ${local.bridge_table_name}
        region: ${local.bridge_region}
    emit:
      telemetry:
        otlp:
          endpoint: https://your-otlp-gateway/otlp
          instance_id: "123456"
          token: $${GC_OTLP_TOKEN}
    identity:
      service_namespace: genai-otel-bridge
      deployment_environment: prod
    sources:
      - type: portkey
        enabled: true
        base_url: https://api.portkey.ai/v1
        auth:
          value: $${PORTKEY_API_KEY}
  EOT

  # Secrets Manager ARNs — injected as env vars; reference them in config_yaml as $${MY_ENV_NAME}.
  secret_arns = {
    GC_OTLP_TOKEN   = aws_secretsmanager_secret.gc_token.arn
    PORTKEY_API_KEY = aws_secretsmanager_secret.portkey.arn
  }

  tags = { env = "prod", team = "platform" }
}

output "table_name" {
  value = module.genai_otel_bridge.table_name
}
```

### Minimum required `config_yaml` HA block

The config passed via `config_yaml` must set the DynamoDB HA backend so the tasks elect a leader
and write checkpoints to the table this module creates:

```yaml
ha:
  coordinator: dynamodb
  checkpoint: dynamodb
  dynamodb:
    table: genai-otel-bridge-ha   # must match var.name + "-ha" (this module's table name)
    region: eu-west-1             # must match var.region
    # Optional overrides (defaults shown):
    # lock_name: genai-otel-bridge-leader
    # lease_duration: 15s
    # renew_deadline: 10s
    # retry_period: 2s
```

Full config schema: `internal/config/config.go` in the repo.

---

## Image override (GHCR proxy / ECR mirror)

The default image is `ghcr.io/rknightion/genai-otel-bridge:latest` (public GHCR, no credentials
needed). Override `var.image` to point at:

- **A GHCR pull-through cache** (e.g. via a CodeArtifact or Artifactory proxy):
  ```hcl
  image = "my-proxy.example.com/rknightion/genai-otel-bridge:v3.0.0"
  ```

- **An ECR mirror** (avoids GHCR egress; recommended for prod ECS on VPC endpoints):
  ```hcl
  image = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/genai-otel-bridge:v3.0.0"
  ```
  If you use ECR, add an execution-role policy for `ecr:GetAuthorizationToken` + `ecr:BatchGetImage`
  on the mirror repo, and add a VPC endpoint for ECR if you want to avoid NAT egress.

- **A specific digest** (pinned, immutable deployment):
  ```hcl
  image = "ghcr.io/rknightion/genai-otel-bridge:v3.0.0@sha256:abc123..."
  ```

---

## Fargate vs EC2 (`launch_type`)

| | FARGATE (default) | EC2 |
|---|---|---|
| Infrastructure | Serverless (no nodes to manage) | You manage EC2 instances / ASG |
| Cost | Per-vCPU-second; Graviton ~20% cheaper | EC2 instance pricing (idle nodes cost money) |
| stopTimeout | **120 s hard cap** (AWS limit) | No cap — set longer drain if needed |
| Graviton | Auto-selected from multi-arch image | Requires arm64 instance type |
| Recommended | Yes — default | For shops with existing EC2 ECS clusters |

```hcl
# EC2 launch type:
launch_type = "EC2"
```

**Fargate stopTimeout note:** The module sets `stopTimeout = 120` s (camelCase, as required by the container-definition schema). This gives
the active leader up to 120 s after SIGTERM to finish the in-flight emit and write the final
checkpoint before SIGKILL. The DynamoDB lock is deliberately NOT released on shutdown (it expires
naturally, matching the Kubernetes coordinator's `ReleaseOnCancel=false` behaviour). The standby task
acquires the lock within approximately `lease_duration` (~15 s default) after expiry. If your emit
retry budget could exceed 120 s (e.g. very aggressive retry settings), tune down `emit.retry_max`
in your config to fit within the window.

On **EC2** there is no 120 s cap. The task's `stopTimeout` still defaults to 120 s in this module
for parity, but you can override it in a root-module `container_definitions` block if needed.

---

## Networking and VPC endpoints (production hardening)

The security group allows egress on:
- TCP 443 — OTLP endpoint, vendor APIs (Portkey, LangSmith), DynamoDB public endpoint
- UDP/TCP 53 — DNS

For production deployments, consider adding VPC endpoints to avoid NAT egress costs and to tighten
the security group rules:
- **DynamoDB gateway endpoint** — free; routes DynamoDB traffic inside the VPC (remove the 443 rule
  for `amazonaws.com` and replace with a prefix-list rule for the DynamoDB endpoint).
- **Secrets Manager interface endpoint** — routes Secrets Manager calls inside the VPC.
- **ECR interface endpoints** (`api.ecr`, `dkr.ecr`, `s3`) — if using an ECR image mirror.

---

## Inputs

| Name | Type | Default | Description |
|------|------|---------|-------------|
| `name` | string | `"genai-otel-bridge"` | Name prefix for all resources |
| `region` | string | — | AWS region |
| `vpc_id` | string | — | VPC ID |
| `subnet_ids` | list(string) | — | Subnet IDs (>= 2 AZs) |
| `image` | string | `ghcr.io/rknightion/genai-otel-bridge:latest` | Container image |
| `launch_type` | string | `"FARGATE"` | `FARGATE` or `EC2` |
| `cpu` | number | `256` | Task CPU units |
| `memory` | number | `512` | Task memory (MiB) |
| `desired_count` | number | `2` | Number of tasks (active/standby) |
| `secret_arns` | map(string) | `{}` | env name → Secrets Manager ARN |
| `kms_key_arn` | string | `null` | CMK ARN for DynamoDB SSE (null = AWS-owned key) |
| `config_yaml` | string | — | Rendered app config YAML |
| `tags` | map(string) | `{}` | Tags for all resources |

## Outputs

| Name | Description |
|------|-------------|
| `table_name` | DynamoDB table name — set as `ha.dynamodb.table` in config |
| `table_arn` | DynamoDB table ARN |
| `cluster_arn` | ECS cluster ARN |
| `cluster_name` | ECS cluster name |
| `service_name` | ECS service name |
| `service_id` | ECS service ARN |
| `security_group_id` | Egress security group ID |
