# GitOps: SLO-driven Canary Operator

This repository contains a Go-based Kubernetes Operator (controller-runtime) that automates progressive delivery (10% → 50% → 100%) using Prometheus SLOs with automatic rollback on breach. It also includes Helm charts, Kustomize overlays, ArgoCD manifests, Gatekeeper/OPA policies, k6 load tests, Grafana dashboards, and runbooks.

Highlights
- Go controller watching a `Canary` CRD.
- Progressive traffic shifting with SLO guardrails (p95 latency, error rate, error budget).
- Rollback within ~30s on regression.
- GitOps-friendly: Helm chart + Kustomize overlays + ArgoCD.
- Policies via Gatekeeper for image signing and resource limits.

Quick Start
- See `charts/canary-operator` for Helm deployment.
- See `config/overlays/*` for Kustomize overlays per env.
- See `argocd/*` for ArgoCD Apps.
- Apply Gatekeeper policies in `policies/gatekeeper`.
- Run load tests from `loadtest/k6`.

## Local validation

The controller can be tested without a Kubernetes cluster. From `operator/`, run
`go test ./...` and `go vet ./...`. From the repository root, run `helm lint
charts/canary-operator` and render each directory under `config/overlays` with
`kustomize build`.

The controller samples Prometheus on every reconciliation so an SLO breach can
roll back early, but it will not advance a rollout step until `stepInterval` has
elapsed. Existing Services are patched by annotation and are never adopted as
children of the Canary resource.
