// 1b. Overhead, 100% bypass traffic (temperature > 0 is never cached).
//     k6 run loadtest/bypass.js
import http from 'k6/http';
import { check } from 'k6';
import { BASE, PARAMS, chat } from './common.js';

const body = chat('always bypass', 0.7);

export const options = {
  vus: Number(__ENV.VUS || 50),
  duration: __ENV.DURATION || '60s',
  thresholds: { http_req_failed: ['rate==0'] },
};

export default function () {
  const r = http.post(`${BASE}/v1/chat/completions`, body, PARAMS);
  check(r, { '200': (x) => x.status === 200, 'BYPASS': (x) => x.headers['X-Cache'] === 'BYPASS' });
}
