# Benchmarks

Target publishables
- Mean rollout time at target traffic (10%→50%→100%).
- Rollback latency on failure.

Methodology
- Use `loadtest/k6/canary_smoke.js` to drive traffic.
- Configure Canary CR with `stepInterval` (e.g., 30s) and thresholds.
- Measure timestamps from status transitions and operator logs.

Data to capture
- For each run: step timestamps, p95, error rate, error budget remaining.
- Rollback scenario: time from SLO breach to full rollback (stable=100, canary=0).

Results Template
- Mean rollout time: <x>s (n=<runs>)
- 95th percentile rollback latency: <y>s

