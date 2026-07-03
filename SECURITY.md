# Security Policy

## Supported versions

`genai-otel-bridge` follows semantic versioning. Security fixes are applied to the latest
released `3.x` minor and shipped in a new patch release. Older majors are not maintained.

| Version | Supported          |
| ------- | ------------------ |
| 3.x     | :white_check_mark: |
| < 3.0   | :x:                |

## Reporting a vulnerability

Please **do not open a public issue** for security vulnerabilities.

Report privately via GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository (**Security → Report a vulnerability**), including the details and, if possible, a
minimal reproduction.

You can expect an acknowledgement within a few business days. We will keep you informed of
progress, agree a disclosure timeline with you, and credit you in the release notes unless you
prefer to remain anonymous.

## Scope notes

`genai-otel-bridge` is **content-free by design**: it never requests prompt/response bodies from the
platform APIs it polls, and an outbound field allow/deny-list governs every emitted field. Reports
that demonstrate a way to make the service emit inference content, leak credentials, or bypass the
egress/SSRF guards are especially in scope.
