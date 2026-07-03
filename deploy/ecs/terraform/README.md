# genai-otel-bridge — ECS Terraform module

Reusable Terraform module that deploys `genai-otel-bridge` on AWS ECS (Fargate default, EC2 optional)
with full active/standby leader-elected HA backed by DynamoDB — the same single-emit, gap-free model
as the Kubernetes/Helm deployment.

Built on [terraform-aws-modules](https://registry.terraform.io/namespaces/terraform-aws-modules):
- `terraform-aws-modules/dynamodb-table/aws ~> 5.0` — lock + checkpoint table
- `aws_security_group` (raw resource) — default-deny egress security group
- `terraform-aws-modules/ecs/aws ~> 7.0` — ECS cluster
- `terraform-aws-modules/ecs/aws//modules/service ~> 7.0` — ECS service (2 tasks active/standby)

One multi-arch image (`amd64` + `arm64`/Graviton) runs on both ECS and Kubernetes — no separate build.

---

## Quick-start usage

By default the module **consumes its own bundled, schema-generated config** (`config.example.yaml`) —
you don't supply `config_yaml` at all. The simplest working deployment is just the table-backing knobs
plus your secrets:

```hcl
# This is a reusable module with NO provider block of its own — configure the aws provider in your
# root module (the module inherits it). That's what lets it compose with count/for_each.
provider "aws" {
  region = "eu-west-1"
}

module "genai_otel_bridge" {
  source = "github.com/rknightion/genai-otel-bridge//deploy/ecs/terraform"

  name       = "genai-otel-bridge"
  vpc_id     = "vpc-0abc123"
  subnet_ids = ["subnet-0aaa", "subnet-0bbb"] # >= 2 AZs for active/standby spread

  # config_yaml is OMITTED → the module injects its bundled config.example.yaml (generated from the Go
  # schema by `make generate`, drift-gated in CI — see "Generated reference config" below), with
  # ha.dynamodb.table auto-rewritten to "<name>-ha". That default runs one Portkey source (analytics)
  # with DynamoDB-backed HA. deployment_environment resolves ${ENV}; aws_region lets the DynamoDB SDK
  # resolve the region (the default config omits ha.dynamodb.region).
  deployment_environment = "prod"
  aws_region             = "eu-west-1"

  # Secrets Manager ARNs — injected as env vars; the bundled default config references these names.
  # (The literal ${MY_ENV_NAME} in the YAML reaches the binary, which resolves it from the injected
  # env var at load time.) These are fetched by the ECS agent using the module-created EXECUTION role
  # at task launch (the module wires task_exec_secret_arns = values(secret_arns) for you) — NOT by the
  # task/app role, which never calls Secrets Manager. If the secrets are encrypted with a CUSTOMER-managed
  # KMS key, also grant kms:Decrypt on that key to the execution role via task_exec_iam_statements.
  secret_arns = {
    GC_OTLP_ENDPOINT = aws_secretsmanager_secret.gc_endpoint.arn
    GC_INSTANCE_ID   = aws_secretsmanager_secret.gc_instance.arn
    GC_OTLP_TOKEN    = aws_secretsmanager_secret.gc_token.arn
    PORTKEY_API_KEY  = aws_secretsmanager_secret.portkey.arn
  }

  tags = { env = "prod", team = "platform" }
}

output "table_name" {
  value = module.genai_otel_bridge.table_name
}

# Inspect the exact config the tasks run (placeholders, no secrets):
output "rendered_config" {
  value = module.genai_otel_bridge.effective_config_yaml
}
```

To run a **different** config (extra sources/loops, the second vendor, custom governance), set
`config_yaml` explicitly — copy `config.example.yaml` as a starting point, set `ha.dynamodb.table` to
`<name>-ha`, and pass it via `config_yaml = file("…")`. Anything you pass overrides the bundled default.

### Generated reference config (`config.example.yaml`)

`config.example.yaml` is **generated** from the Go config schema (`internal/config/config.go`) by
`make generate` — the same source of truth and the same generator that produce the Helm chart's
`values.yaml` `config:` block, but rendered under the **ECS profile** (`ha.coordinator`/`ha.checkpoint` =
`dynamodb` and the `ha.dynamodb` block included at its defaults, which the chart omits). A drift gate
(`TestECSConfigExampleUpToDate`) fails CI if a schema change is not regenerated and committed, so this
file can never silently fall out of sync with the binary's accepted config — every current setting
appears at its default with the field's inline doc-comment. The module injects it as the default
`config_yaml` (table rewritten to `<name>-ha`). **Never hand-edit it**; override `var.config_yaml`
instead, or copy it as a starting point.

The active config runs one Portkey source (analytics loop) — the only shape that starts cleanly with
just credentials. The file also carries a **commented all-loops/both-vendor example block** (Portkey
`groups`/`logs_export`, LangSmith `sessions`/`runs`, from each source's `ExampleSource()`) showing the
full surface. It is commented because `groups`/`logs_export`/LangSmith need real per-deployment settings
(`workspace_id`, `signed_url_allow_hosts`, project scope, LangSmith creds) that have no functional
universal default. Its env-refs are written as `<VAR>` placeholders, not `${VAR}` — the whole file is
parsed by the binary (which resolves `${VAR}` even in comments), so live refs would force those env vars
on every default deploy. To enable a loop: uncomment it into `config.sources`, restore the `${VAR}`
refs, and supply the secrets/settings.

### Minimum required `config_yaml` HA block

The bundled `config.example.yaml` already contains this block at its defaults (and the module rewrites
`table` to `<name>-ha`) — you only need this when supplying your OWN `config_yaml`:

```yaml
ha:
  coordinator: dynamodb
  checkpoint: dynamodb
  dynamodb:
    table: genai-otel-bridge-ha   # must match var.name + "-ha" (this module's table name)
    region: eu-west-1             # or omit and set var.aws_region (injected as AWS_REGION)
    # Optional overrides (defaults shown):
    # lock_name: genai-otel-bridge-leader
    # lease_duration: 15s
    # renew_deadline: 10s
    # retry_period: 2s
```

**NTP note:** unlike the Kubernetes Lease backend, failover timing here compares the candidate task's
wall clock against an `expiresAtMs` written from the leader task's wall clock — absolute inter-node
clock skew (not just drift) affects how quickly a candidate can take over, bounded by roughly
`lease_duration - renew_deadline`. Run an NTP-synced base image/task (ECS-optimized AMIs and Fargate
both sync by default) rather than disabling clock sync. See `ARCHITECTURE.md` decision ledger #17.

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
acquires the lock within approximately `lease_duration` (~15 s default) after expiry. Note the emit
retry budget is a fixed constant (`DefaultRetryPolicy`, `MaxElapsed = 5 min`) — it is **not**
config-tunable, and it already exceeds the 120 s Fargate `stopTimeout` by default. In practice this
means a retry sequence in progress at SIGTERM is hard-cancelled at the 120 s mark rather than running
to exhaustion; this is safe (no watermark advances past uncommitted work, and the next leader re-emits
deterministically). If you want the leader to have longer to finish before SIGKILL, use the EC2 launch
type (no 120 s cap) or override `stopTimeout` in a root-module `container_definitions` block.

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
| `vpc_id` | string | — | VPC ID |
| `subnet_ids` | list(string) | — | Subnet IDs (>= 2 AZs) |
| `image` | string | `ghcr.io/rknightion/genai-otel-bridge:latest` | Container image |
| `launch_type` | string | `"FARGATE"` | `FARGATE` or `EC2` |
| `cpu` | number | `256` | Task CPU units |
| `memory` | number | `512` | Task memory (MiB) |
| `desired_count` | number | `2` | Number of tasks (active/standby) |
| `secret_arns` | map(string) | `{}` | env name → Secrets Manager ARN |
| `kms_key_arn` | string | `null` | CMK ARN for DynamoDB SSE (null = AWS-owned key) |
| `config_yaml` | string | `null` | Rendered app config YAML. `null` ⇒ use the bundled generated `config.example.yaml` (table rewritten to `<name>-ha`) |
| `deployment_environment` | string | `"ecs"` | Injected as the `ENV` env var (resolves `${ENV}` in the config) |
| `aws_region` | string | `null` | Injected as `AWS_REGION` so the DynamoDB SDK resolves the region (the bundled config omits `ha.dynamodb.region`) |
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
| `effective_config_yaml` | The config actually injected (var.config_yaml or the bundled default with table rewritten) — placeholders, no secrets |
