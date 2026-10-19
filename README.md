# llm-cache-go

[![ci](https://github.com/kodurshravya/llm-cache-go/actions/workflows/ci.yml/badge.svg)](https://github.com/kodurshravya/llm-cache-go/actions/workflows/ci.yml)

An OpenAI-compatible caching proxy for LLM traffic, written in Go. It sits in front of any OpenAI-style endpoint (OpenAI, Azure OpenAI, or my Python LLM API Gateway) and serves repeated requests from Redis instead of paying for them again.

Three things make it more than a key-value wrapper:

1. **Canonical cache keys.** Two requests that mean the same thing hit the same entry, even if JSON field order or whitespace differs.
2. **Versioned invalidation.** Changing a prompt template invalidates only that template's entries, in O(1), without scanning Redis.
3. **Request coalescing.** 50 identical requests arriving at once make 1 upstream call; the other 49 wait and share the result.

> Status: in progress. Numbers in the Results section are filled in only from my own runs, with the method described next to them.

---

## Architecture

```
client ──► llm-cache-go ──(miss)──► upstream (OpenAI / Azure / Python gateway)
              │   ▲
              ▼   │
             Redis (entries + template versions)
```

Request path:

1. Parse the request and decide: **cacheable**, or **bypass** (see Cache policy).
2. Build the key: `tenant | model | template@version | sha256(canonical request)`.
3. `GET` from Redis. Hit: return it (JSON or replayed SSE stream) with `X-Cache: HIT`.
4. Miss: enter a singleflight group on the key. One goroutine calls upstream; the rest wait.
5. Store the response only if it succeeded and finished cleanly, with the model's TTL.
6. Return with `X-Cache: MISS` (or `COALESCED` for the waiters).

If Redis is unreachable, the proxy fails open: requests are forwarded as `BYPASS` instead of erroring.

## API

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/chat/completions` | Cached proxy (JSON and `stream: true`) |
| POST | `/v1/embeddings` | Cached proxy (embeddings are deterministic, so they're always cacheable) |
| GET | `/healthz` | Liveness: process up |
| GET | `/readyz` | Readiness: Redis and upstream reachable |
| GET | `/metrics` | Prometheus metrics |
| POST | `/admin/templates/{name}/bump` | Invalidate one prompt template (increments its version) |
| DELETE | `/admin/cache?model={model}` | Flush one model's namespace (bumps a model generation counter) |

Request headers the proxy understands:

| Header | Meaning |
|---|---|
| `Authorization` | Passed through to upstream; also hashed into the tenant namespace (`api-key` is used if `Authorization` is absent) |
| `X-Prompt-Template` | Template name, for example `invoice-audit-v2`; its current version goes into the key |
| `X-Cache-Control` | `no-cache` (skip read, still write), `no-store` (skip both), `force` (cache even when temperature > 0); values can be combined, e.g. `force, no-cache` |

Response headers: `X-Cache: HIT | MISS | COALESCED | BYPASS`, `X-Cache-Key` (first 12 hex chars, for debugging), `X-Cache-Age` (seconds since stored).

## Cache policy

Cached by default:

- `temperature == 0`, or `X-Cache-Control: force` (a missing temperature means OpenAI's default of 1, so it is not cached)
- all embeddings requests

Never cached:

- non-2xx upstream responses and timeouts
- `finish_reason` of `length` or `content_filter`
- streams that disconnect before `[DONE]`
- responses larger than `MAX_ENTRY_BYTES`
- requests with `n > 1`

## Cache key design

```
llmc:{tenant}:{model}:g{modelGen}:{template}@{templateVer}:{sha256(canonical)}
```

- **tenant** = first 16 hex chars of `sha256(api_key)`. Two customers sending the same prompt never share entries. The raw key is never stored or logged.
- **canonical** = the request re-serialized with sorted object keys, insignificant whitespace removed, numbers normalized (`0` and `0.0` match), and only the fields that change the output. Chat: `model, messages, tools, tool_choice, parallel_tool_calls, response_format, temperature, top_p, max_tokens, max_completion_tokens, seed, stop, frequency_penalty, presence_penalty, logit_bias, logprobs, top_logprobs, reasoning_effort`. Embeddings: `model, input, dimensions, encoding_format`. Fields like `user`, `stream` or `stream_options` are excluded, so a streamed and a non-streamed request share an entry.
- **templateVer** comes from `GET llmc:tmplver:{template}` (default 0). Bumping it means old keys are never read again and simply expire by TTL. No `SCAN`, no `DEL` storms. The model generation and template version are read in one `MGET`.

## Streaming

- **Miss:** the proxy forwards SSE chunks to the client as they arrive and tees them into a buffer. After `[DONE]` with a clean `finish_reason`, it stores the assembled response (content, refusals, tool calls, usage).
- **Hit:** the stored response is replayed as SSE chunks in the same format (`chat.completion.chunk` events, then `data: [DONE]`), so clients don't need to know it was cached.
- **Coalesced waiters on a stream:** they receive the replay once the leader finishes. (Stretch goal: fan out live chunks to waiters as they arrive.)
- If the leader's client disconnects mid-stream, the proxy keeps reading upstream so the waiters still get an answer and it can still be cached.

## Coalescing

- **In one process:** `golang.org/x/sync/singleflight` keyed on the cache key. ✅
- **Across replicas (stretch, not built yet):** `SET llmc:lock:{key} NX PX 30000`. Followers poll the entry with backoff until it appears or the lock expires, then fall back to calling upstream themselves.
- If the leader fails, the waiters get the error too; they do not stampede upstream. The next request retries.

## Configuration

Env vars, with defaults:

```
LISTEN_ADDR=:8080
UPSTREAM_URL=https://api.openai.com
REDIS_URL=redis://localhost:6379/0
DEFAULT_TTL=24h
MODEL_TTLS=gpt-4.1=72h,text-embedding-3-small=720h
MAX_ENTRY_BYTES=1048576
UPSTREAM_TIMEOUT=60s
ADMIN_TOKEN=            # required for /admin routes
LOG_LEVEL=info
```

## Project layout

```
cmd/llmcache/         main: config, wiring, graceful shutdown
cmd/fakeupstream/     deterministic fake OpenAI server with fixed latency, for tests and load tests
internal/keys/        canonicalization + key building
internal/cache/       Redis store, TTLs, template/model versions
internal/proxy/       HTTP handlers, policy, singleflight, SSE tee/replay
internal/metrics/     Prometheus collectors
loadtest/             k6 scripts, trace generator (gentrace/), results/
deploy/               Dockerfile, docker-compose.yml (proxy + redis + fakeupstream + prometheus)
.github/workflows/    CI: go vet, golangci-lint, go test -race
```

## Metrics

| Metric | Type |
|---|---|
| `llmc_requests_total{result="hit\|miss\|coalesced\|bypass"}` | counter |
| `llmc_upstream_requests_total{status}` | counter |
| `llmc_upstream_seconds` | histogram |
| `llmc_overhead_seconds` (time spent in the proxy, excluding upstream) | histogram |
| `llmc_entry_bytes` | histogram |
| `llmc_inflight_coalesced` | gauge |

## Tests

```bash
make test        # go test -race -count=1 ./...
```

Unit tests use `miniredis` and an `httptest` upstream that counts calls.

- **Key canonicalization (table-driven):** reordered fields produce the same key; whitespace changes produce the same key; changes to temperature, a tool, or a message produce different keys; `stream` doesn't change the key.
- **Coalescing:** 50 goroutines send the same request against an upstream with 200ms latency, and the upstream call count is 1.
- **Invalidation:** cache, bump the template, and the next request is a MISS; other templates are still HITs. Same for model flush.
- **Tenant isolation:** the same prompt with two API keys produces two upstream calls.
- **Policy:** 500s, `content_filter`, `length`, truncated streams, oversized entries and `n > 1` are never stored.
- **Streaming:** a replayed stream, re-assembled, equals the original message content; tool-call deltas round-trip exactly.
- **Race detector:** the whole suite runs under `go test -race`.

Integration (optional, not built yet): `testcontainers-go` with real Redis.

## Load test method

So the numbers measure the proxy rather than OpenAI's latency, load tests run against `cmd/fakeupstream`, which returns a fixed response after a fixed delay (default 300ms).

1. **Overhead:** 100% hit traffic (`loadtest/hit.js`), then 100% bypass traffic (`loadtest/bypass.js`). Report the p50/p99 of `llmc_overhead_seconds`.
2. **Hit rate on a replayed trace:** `go run ./loadtest/gentrace` produces N requests drawn from a Zipf distribution over M distinct prompts, and prints the best hit rate any cache could reach on it. This is a synthetic trace, and the README says so. Replay with `loadtest/trace.js`. Report hit rate and upstream calls saved.
3. **Throughput comparison:** the same machine, same fake upstream, same trace against (a) this proxy and (b) my Python LLM API Gateway (`BASE_URL=... k6 run loadtest/trace.js`). Report max req/s at 0 errors.
4. **Burst coalescing:** 200 identical concurrent requests on a cold key (`BURST_ID=$(date +%s) k6 run loadtest/burst.js`). Report upstream calls (the script prints them from `fakeupstream`'s `/stats`).

Tooling: k6 (`loadtest/*.js`); results committed as JSON plus a short summary in `loadtest/results/`.

## Results

Fill in only from real runs. Record machine specs, Go version, and the command used.

| Measurement | Value | How measured |
|---|---|---|
| Proxy overhead p50 / p99 (hit) | _TBD_ | k6, 100% hit, fakeupstream 300ms |
| Proxy overhead p50 / p99 (bypass) | _TBD_ | k6, 100% bypass |
| Hit rate on synthetic Zipf trace | _TBD_ | N=_, M=_, s=_ |
| Upstream calls saved | _TBD_ | from `llmc_upstream_requests_total` |
| Max req/s at 0 errors (Go proxy) | _TBD_ | k6 ramping arrival rate |
| Max req/s at 0 errors (Python gateway, same setup) | _TBD_ | same script |
| Burst of 200 on a cold key: upstream calls | _TBD_ | expected 1 per replica |

## Quick start

```bash
docker compose -f deploy/docker-compose.yml up --build

curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4.1-mini","temperature":0,"messages":[{"role":"user","content":"hi"}]}' -i | grep X-Cache
# run it twice: MISS, then HIT

# invalidate a template (the compose file sets ADMIN_TOKEN=dev-admin-token)
curl -s -X POST localhost:8080/admin/templates/invoice-audit-v2/bump -H "Authorization: Bearer dev-admin-token"
```

Without Docker: `make fake` in one terminal, a local Redis, and `make run` in another.

## Build plan (6 days)

| Day | Deliverable |
|---|---|
| 1 | Repo, `fakeupstream`, pass-through proxy, `/healthz`, CI with `go test -race` |
| 2 | `internal/keys` canonicalization + table tests; Redis store with TTLs; JSON hit/miss |
| 3 | Singleflight coalescing + test; cache policy rules + tests; tenant isolation |
| 4 | Template/model versioning + admin routes; SSE tee on miss and replay on hit |
| 5 | Prometheus metrics, docker-compose, k6 scripts, trace generator |
| 6 | Run the load tests, fill in Results, write "Design decisions" below, tag v0.1.0 |

## Design decisions

_(Write these yourself as you go: interviewers ask about them.)_

- Why versioned keys instead of deleting entries
- Why singleflight in-process first, Redis lock second
- Why `stream` is excluded from the key
- What you would change for semantic (embedding-similarity) caching, and why it isn't the default (false hits change answers)

## Resume bullet (fill from Results only)

> Built an OpenAI-compatible LLM response cache in Go with Redis, template-versioned invalidation, and single-flight request coalescing; [X]% hit rate on a replayed trace, [Y]ms p99 overhead, [Z]x the throughput of my Python gateway under the same load test.
