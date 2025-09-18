# Service Level Objectives (SLO) for Canary Rollouts

Metrics
- p95 latency (ms): derived from Prometheus histogram (converted to ms).
- Error rate: 5xx rate / total rate.

Default Queries (examples)
- p95: `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{job="app"}[5m])) by (le)) * 1000`
- error rate: `sum(rate(http_requests_total{job="app",code=~"5.."}[5m])) / sum(rate(http_requests_total{job="app"}[5m]))`

Thresholds
- p95LatencyMsMax: 300 (example)
- errorRateMax: 0.02 (example)

Error Budget
- Remaining = 100% * max(0, 1 - errorRate / errorRateMax)
- Abort if remaining < `abort.minErrorBudgetPercent`.

Operator Behavior
- On each step, queries Prometheus, compares to thresholds.
- If breached, sets stable=100 / canary=0 and marks status=Failed.
- Otherwise proceeds to next step after `stepInterval`.

