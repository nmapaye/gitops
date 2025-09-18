# Runbook: Canary Rollouts

Use this runbook to diagnose and operate SLO-driven canaries.

Pre-checks
- Confirm CRD `canaries.canary.example.io` is installed.
- Confirm Prometheus endpoint is reachable from the operator.
- Confirm stable and canary Services exist.

CR fields
- spec.targetRef: Target Deployment name.
- spec.stableService/spec.canaryService: Services used for routing; operator annotates `canary.example.io/weight`.
- spec.steps: Percent weights e.g., [10,50,100].
- spec.stepInterval: Duration between steps.
- spec.slo: Prometheus URL and queries; thresholds.
- spec.abort: Error budget and latency increase guardrails.

Common operations
- Pause progression: remove next steps or set stepInterval to a large value; apply CR update.
- Force rollback: set steps to [0]; operator will set canary weight 0 and stable 100 on next reconcile.
- Tune thresholds: adjust spec.slo.* and spec.abort.*; reconcile will pick up next loop.

Troubleshooting
- Operator logs: `kubectl logs deploy/canary-operator -n canary-system`.
- Status fields: `kubectl get canary <name> -o yaml` to view `status`.
- Metrics fetch errors: verify PrometheusURL and queries.

SLO breach response
- Operator sets stable 100%, canary 0% within ~one reconcile interval (<30s by default).

