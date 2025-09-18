Load testing with k6

Scenarios
- Smoke canary test driving gradually increased RPS.
- Exports Prometheus metrics via k6 Prometheus Remote Write or summary output for SLO calc.

Run
- Install k6: https://k6.io
- Configure `TARGET_HOST` env, then:
  k6 run k6/canary_smoke.js

SLO Calculation
- p95 latency and error rate are reported by k6; Prometheus queries in the Canary CR should align with these series.
- Use `docs/SLO.md` for the formula and error-budget handling.

