output "table_name" {
  description = "Name of the DynamoDB table used for leader election lock and checkpoint storage. Set as ha.dynamodb.table in the app config."
  value       = module.table.dynamodb_table_id
}

output "table_arn" {
  description = "ARN of the DynamoDB HA table (lock + checkpoint). Used in the task IAM policy."
  value       = module.table.dynamodb_table_arn
}

output "cluster_arn" {
  description = "ARN of the ECS cluster."
  value       = module.cluster.cluster_arn
}

output "cluster_name" {
  description = "Name of the ECS cluster."
  value       = module.cluster.cluster_name
}

output "service_name" {
  description = "Name of the ECS service (active/standby tasks)."
  value       = module.service.name
}

output "service_id" {
  description = "ARN that identifies the ECS service."
  value       = module.service.id
}

output "security_group_id" {
  description = "ID of the egress-only security group attached to the ECS tasks."
  value       = aws_security_group.this.id
}

output "effective_config_yaml" {
  description = "The config actually injected as GENAI_OTEL_BRIDGE_CONFIG — either var.config_yaml or the bundled generated config.example.yaml with the table rewritten to <name>-ha. Contains $${ENV}-style placeholders, no secret values. Useful to verify what the tasks run."
  value       = local.effective_config
}
