package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kodurshravya/llm-cache-go/internal/cache"
	"github.com/kodurshravya/llm-cache-go/internal/metrics"
)

const (
	chatBody   = `{"model":"gpt-test","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	streamBody = `{"model":"gpt-test","temperature":0,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	adminToken = "admin-secret"
)

// ---------- test environment ----------

type env struct {
	proxy *httptest.Server
	calls *atomic.Int64
}

func newEnv(t *testing.T, upstream http.HandlerFunc) *env {
	t.Helper()
	calls := new(atomic.Int64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(up.Close)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		UpstreamURL:     u,
		UpstreamTimeout: 5 * time.Second,
		MaxEntryBytes:   1 << 20,
		AdminToken:      adminToken,
	}, cache.New(rdb, time.Hour, nil), metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	p := httptest.NewServer(s.Handler())
	t.Cleanup(p.Close)
	return &env{proxy: p, calls: calls}
}

// do sends a request. headers are name/value pairs and override defaults.
func (e *env) do(t *testing.T, method, path, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tenant-a")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func (e *env) chat(t *testing.T, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/chat/completions", body, headers...)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// completionHandler answers JSON or SSE depending on the request's stream flag.
func completionHandler(content, finish string, latency time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			writeFakeStream(w, content, finish, true)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finish,
			}},
		}))
	}
}

func writeFakeStream(w http.ResponseWriter, content, finish string, done bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	send := func(delta map[string]any, fin any) {
		b := mustMarshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		})
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		fl.Flush()
	}
	send(map[string]any{"role": "assistant", "content": ""}, nil)
	for _, piece := range strings.SplitAfter(content, " ") {
		send(map[string]any{"content": piece}, nil)
	}
	send(map[string]any{}, finish)
	if done {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}
}

// sseContent reassembles the message content from an SSE body.
func sseContent(t *testing.T, body string) (content string, done bool) {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		p = strings.TrimSpace(p)
		if p == "[DONE]" {
			done = true
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &ch); err != nil {
			t.Fatalf("bad chunk %q: %v", p, err)
		}
		for _, c := range ch.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
	return sb.String(), done
}

func jsonContent(t *testing.T, body string) string {
	t.Helper()
	var c completion
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("bad completion %q: %v", body, err)
	}
	if len(c.Choices) == 0 || c.Choices[0].Message.Content == nil {
		t.Fatalf("no content in %q", body)
	}
	return *c.Choices[0].Message.Content
}

func expectCache(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	if got := resp.Header.Get("X-Cache"); got != want {
		t.Fatalf("X-Cache = %q, want %q", got, want)
	}
}

func expectCalls(t *testing.T, e *env, want int64) {
	t.Helper()
	if got := e.calls.Load(); got != want {
		t.Fatalf("upstream calls = %d, want %d", got, want)
	}
}

// ---------- tests ----------

func TestMissThenHit(t *testing.T) {
	e := newEnv(t, completionHandler("hello world", "stop", 0))

	r1, b1 := e.chat(t, chatBody)
	expectCache(t, r1, "MISS")
	r2, b2 := e.chat(t, chatBody)
	expectCache(t, r2, "HIT")

	if b1 != b2 {
		t.Fatalf("hit body differs:\n%s\n%s", b1, b2)
	}
	if len(r2.Header.Get("X-Cache-Key")) != 12 {
		t.Fatalf("X-Cache-Key = %q", r2.Header.Get("X-Cache-Key"))
	}
	if r2.Header.Get("X-Cache-Age") == "" {
		t.Fatal("missing X-Cache-Age on hit")
	}
	expectCalls(t, e, 1)
}

func TestReorderedRequestHits(t *testing.T) {
	e := newEnv(t, completionHandler("hello", "stop", 0))
	e.chat(t, chatBody)
	r, _ := e.chat(t, `{ "messages": [{"content":"hi","role":"user"}], "temperature": 0.0, "model": "gpt-test", "user": "x" }`)
	expectCache(t, r, "HIT")
	expectCalls(t, e, 1)
}

func TestCoalescing(t *testing.T) {
	e := newEnv(t, completionHandler("slow answer", "stop", 200*time.Millisecond))

	const n = 50
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		mu     sync.Mutex
		counts = map[string]int{}
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, body := e.chat(t, chatBody)
			if resp.StatusCode != http.StatusOK || jsonContent(t, body) != "slow answer" {
				t.Errorf("status %d body %s", resp.StatusCode, body)
			}
			mu.Lock()
			counts[resp.Header.Get("X-Cache")]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	expectCalls(t, e, 1)
	if counts["MISS"] != 1 {
		t.Fatalf("expected exactly one MISS, got %v", counts)
	}
	t.Logf("results: %v", counts)
}

func TestLeaderErrorIsShared(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, _ := e.chat(t, chatBody); resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if got := e.calls.Load(); got > 2 {
		t.Fatalf("waiters stampeded upstream: %d calls", got)
	}
}

func TestTemplateBump(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	tmplA := []string{"X-Prompt-Template", "invoice-audit-v2"}
	tmplB := []string{"X-Prompt-Template", "other"}

	e.chat(t, chatBody, tmplA...)
	e.chat(t, chatBody, tmplB...)
	r, _ := e.chat(t, chatBody, tmplA...)
	expectCache(t, r, "HIT")

	resp, body := e.do(t, http.MethodPost, "/admin/templates/invoice-audit-v2/bump", "",
		"Authorization", "Bearer "+adminToken)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"version":1`) {
		t.Fatalf("bump: %d %s", resp.StatusCode, body)
	}

	r, _ = e.chat(t, chatBody, tmplA...)
	expectCache(t, r, "MISS")
	r, _ = e.chat(t, chatBody, tmplB...)
	expectCache(t, r, "HIT")
}

func TestModelFlush(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	e.chat(t, chatBody)
	resp, _ := e.do(t, http.MethodDelete, "/admin/cache?model=gpt-test", "", "Authorization", "Bearer "+adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("flush status %d", resp.StatusCode)
	}
	r, _ := e.chat(t, chatBody)
	expectCache(t, r, "MISS")
	expectCalls(t, e, 2)
}

func TestAdminRequiresToken(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	resp, _ := e.do(t, http.MethodPost, "/admin/templates/t/bump", "", "Authorization", "Bearer wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	r1, _ := e.chat(t, chatBody, "Authorization", "Bearer tenant-a")
	r2, _ := e.chat(t, chatBody, "Authorization", "Bearer tenant-b")
	expectCache(t, r1, "MISS")
	expectCache(t, r2, "MISS")
	expectCalls(t, e, 2)
}

func TestNotStored(t *testing.T) {
	cases := []struct {
		name      string
		upstream  http.HandlerFunc
		body      string
		headers   []string
		wantCache string // X-Cache on the second request
	}{
		{"upstream 500", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
		}, chatBody, nil, "MISS"},
		{"content_filter", completionHandler("x", "content_filter", 0), chatBody, nil, "MISS"},
		{"length", completionHandler("x", "length", 0), chatBody, nil, "MISS"},
		{"n > 1", completionHandler("x", "stop", 0),
			`{"model":"gpt-test","temperature":0,"n":2,"messages":[{"role":"user","content":"hi"}]}`, nil, "BYPASS"},
		{"temperature > 0", completionHandler("x", "stop", 0),
			`{"model":"gpt-test","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`, nil, "BYPASS"},
		{"temperature missing", completionHandler("x", "stop", 0),
			`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`, nil, "BYPASS"},
		{"no-store", completionHandler("x", "stop", 0), chatBody, []string{"X-Cache-Control", "no-store"}, "BYPASS"},
		{"truncated stream", func(w http.ResponseWriter, _ *http.Request) {
			writeFakeStream(w, "cut off", "stop", false) // no [DONE]
		}, streamBody, nil, "MISS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.upstream)
			e.chat(t, tc.body, tc.headers...)
			r, _ := e.chat(t, tc.body, tc.headers...)
			expectCache(t, r, tc.wantCache)
			expectCalls(t, e, 2)
		})
	}
}

func TestOversizedEntryNotStored(t *testing.T) {
	big := strings.Repeat("a", 2<<20) // larger than MaxEntryBytes (1 MiB)
	e := newEnv(t, completionHandler(big, "stop", 0))
	e.chat(t, chatBody)
	r, _ := e.chat(t, chatBody)
	expectCache(t, r, "MISS")
	expectCalls(t, e, 2)
}

func TestForceCachesNonZeroTemperature(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	body := `{"model":"gpt-test","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
	e.chat(t, body, "X-Cache-Control", "force")
	r, _ := e.chat(t, body, "X-Cache-Control", "force")
	expectCache(t, r, "HIT")
	expectCalls(t, e, 1)
}

func TestNoCacheSkipsReadButWrites(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	e.chat(t, chatBody)
	r, _ := e.chat(t, chatBody, "X-Cache-Control", "no-cache")
	expectCache(t, r, "MISS")
	r, _ = e.chat(t, chatBody)
	expectCache(t, r, "HIT")
	expectCalls(t, e, 2)
}

func TestStreamMissThenReplay(t *testing.T) {
	const want = "streamed answer with several words"
	e := newEnv(t, completionHandler(want, "stop", 0))

	r1, b1 := e.chat(t, streamBody)
	expectCache(t, r1, "MISS")
	if !strings.HasPrefix(r1.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type %q", r1.Header.Get("Content-Type"))
	}
	if got, done := sseContent(t, b1); got != want || !done {
		t.Fatalf("live stream: %q done=%v", got, done)
	}

	r2, b2 := e.chat(t, streamBody)
	expectCache(t, r2, "HIT")
	if got, done := sseContent(t, b2); got != want || !done {
		t.Fatalf("replayed stream: %q done=%v", got, done)
	}

	// stream is not part of the key, so a JSON request shares the entry.
	r3, b3 := e.chat(t, chatBody)
	expectCache(t, r3, "HIT")
	if got := jsonContent(t, b3); got != want {
		t.Fatalf("json from stored stream: %q", got)
	}
	expectCalls(t, e, 1)
}

func TestJSONThenStreamReplay(t *testing.T) {
	e := newEnv(t, completionHandler("from json", "stop", 0))
	e.chat(t, chatBody)
	r, b := e.chat(t, streamBody)
	expectCache(t, r, "HIT")
	if got, done := sseContent(t, b); got != "from json" || !done {
		t.Fatalf("got %q done=%v", got, done)
	}
}

func TestEmbeddingsAlwaysCached(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"emb"}`)
	})
	body := `{"model":"emb","input":"hello"}`
	r1, _ := e.do(t, http.MethodPost, "/v1/embeddings", body)
	r2, _ := e.do(t, http.MethodPost, "/v1/embeddings", body)
	expectCache(t, r1, "MISS")
	expectCache(t, r2, "HIT")
	expectCalls(t, e, 1)
}

func TestBadRequest(t *testing.T) {
	e := newEnv(t, completionHandler("x", "stop", 0))
	for _, body := range []string{`{not json`, `{"temperature":0}`} {
		if r, _ := e.chat(t, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, r.StatusCode)
		}
	}
	expectCalls(t, e, 0)
}
