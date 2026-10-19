// 1. Overhead, 100% hit traffic. Read llmc_overhead_seconds from /metrics after.
//    k6 run loadtest/hit.js
import http from 'k6/http';
import { check } from 'k6';
import { BASE, PARAMS, chat } from './common.js';

const body = chat('warm hit');

export const options = {
  vus: Number(__ENV.VUS || 50),
  duration: __ENV.DURATION || '60s',
  thresholds: { http_req_failed: ['rate==0'] },
};

export function setup() {
  http.post(`${BASE}/v1/chat/completions`, body, PARAMS); // warm the key
}

export default function () {
  const r = http.post(`${BASE}/v1/chat/completions`, body, PARAMS);
  check(r, { '200': (x) => x.status === 200, 'HIT': (x) => x.headers['X-Cache'] === 'HIT' });
}
