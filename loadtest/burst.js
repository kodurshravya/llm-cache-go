// 4. Burst coalescing: 200 identical concurrent requests on a cold key.
//    Use a new BURST_ID each run so the key is cold.
//    BURST_ID=$(date +%s) k6 run loadtest/burst.js
import http from 'k6/http';
import { check } from 'k6';
import { BASE, FAKE, PARAMS, chat } from './common.js';

const body = chat(`cold burst ${__ENV.BURST_ID || '1'}`);

export const options = {
  scenarios: {
    burst: { executor: 'per-vu-iterations', vus: 200, iterations: 1, maxDuration: '30s' },
  },
  thresholds: { http_req_failed: ['rate==0'] },
};

export function setup() {
  return { before: JSON.parse(http.get(`${FAKE}/stats`).body).calls };
}

export default function () {
  const r = http.post(`${BASE}/v1/chat/completions`, body, PARAMS);
  check(r, { '200': (x) => x.status === 200 });
}

export function teardown(data) {
  const after = JSON.parse(http.get(`${FAKE}/stats`).body).calls;
  console.log(`upstream calls during burst: ${after - data.before}`);
}
