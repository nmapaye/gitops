import http from 'k6/http';
import { sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

export let options = {
  stages: [
    { duration: '1m', target: 10 },
    { duration: '2m', target: 50 },
    { duration: '2m', target: 100 },
  ],
  thresholds: {
    http_req_duration: ['p(95)<300'],
    http_req_failed: ['rate<0.02'],
  },
};

const p95 = new Trend('latency_p95_ms');
const errRate = new Rate('errors');

const BASE_URL = __ENV.TARGET_HOST || 'http://localhost:8080';

export default function () {
  const res = http.get(BASE_URL);
  p95.add(res.timings.duration);
  errRate.add(res.status >= 500);
  sleep(0.1);
}

