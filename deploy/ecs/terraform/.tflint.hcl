# tflint configuration for the genai-otel-bridge ECS module.
#
# Uses the BUNDLED "terraform" ruleset only (correctness/hygiene: unused declarations, deprecated
# syntax, module version pinning, comment formatting) — it needs no `tflint --init`, no plugin
# download, and no network/token, so the CI gate stays fast and deterministic.
#
# AWS security posture is covered by checkov (run alongside tflint in `make tf-validate`). The AWS
# provider ruleset (github.com/terraform-linters/tflint-ruleset-aws) can be added later behind
# `tflint --init` if deeper provider-schema validation is wanted.
plugin "terraform" {
  enabled = true
}
