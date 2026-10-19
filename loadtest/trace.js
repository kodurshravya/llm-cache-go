// 2 + 3. Replay a synthetic Zipf trace at a ramping arrival rate.
//   go run ./loadtest/gentrace -n 10000 -m 1000 -s 1.1
//   k6 run loadtest/trace.js                                  # Go proxy
//   BASE_URL=http://localhost:8000 k6 run loadtest/trace.js   # Python gateway, same script
// Flush the cache first (DELETE /admin/cache?model=gpt-4.1-mini) so runs are comparable.
import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { SharedArray } from 'k6/data';
import { BASE, PARAMS } from './common.js';

const trace = new SharedArray('trace', () =>
  open(__ENV.TRACE || './trace.jsonl').split('\n').filter((l) => l.trim() !== ''),
);
const results = {
  HIT: new Counter('cache_hit'),
  MISS: new Counter('cache_miss'),
  COALESCED: new Counter('cache_coalesced'),
  BYPASS: new Counter('cache_bypass'),
};

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: Number(__ENV.START_RATE || 50),
      timeUnit: '1s',
      preAllocatedVUs: 200,
      maxVUs: Number(__ENV.MAX_VUS || 2000),
      stages: [
        { target: Number(__ENV.RATE_1 || 200), duration: '30s' },
        { target: Number(__ENV.RATE_2 || 1000), duration: '60s' },
        { target: Number(__ENV.RATE_3 || 2000), duration: '60s' },
      ],
    },
  },
  thresholds: { http_req_failed: ['rate==0'] },
};

export default function () {
  const body = trace[exec.scenario.iterationInTest % trace.length];
  const r = http.post(`${BASE}/v1/chat/completions`, body, PARAMS);
  check(r, { '200': (x) => x.status === 200 });
  const c = results[r.headers['X-Cache']];
  if (c) c.add(1);
}
