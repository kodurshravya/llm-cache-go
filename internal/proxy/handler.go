package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kodurshravya/llm-cache-go/internal/cache"
	"github.com/kodurshravya/llm-cache-go/internal/keys"
)

// result is one upstream outcome, shared by the leader and every waiter.
// It must be treated as read-only once returned.
type result struct {
	status      int
	contentType string
	body        []byte        // complete JSON (streams are assembled)
	ok          bool          // 2xx and body is a complete response
	delivered   bool          // the leader already streamed it to its own client
	upstream    time.Duration // time spent on the upstream call
	err         error         // transport error or timeout
}

// forwardHeaders are copied to upstream. Api-Key is Azure's auth header.
var forwardHeaders = []string{"Authorization", "Api-Key", "Content-Type", "Accept", "OpenAI-Organization", "OpenAI-Project"}

func credential(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return v
	}
	return r.Header.Get("Api-Key")
}

func (s *Server) cached(kind, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not read request body")
			return
		}
		var info reqInfo
		if err := json.Unmarshal(body, &info); err != nil {
			writeError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
			return
		}
		if info.Model == "" {
			writeError(w, http.StatusBadRequest, "missing model")
			return
		}
		if kind == keys.KindEmbeddings {
			info.Stream = false
		}

		d := decide(kind, info, r.Header.Get("X-Cache-Control"))
		if !d.read && !d.write {
			s.bypass(w, r, path, body, start)
			return
		}

		k, err := s.buildKey(r, kind, body, info.Model)
		if err != nil {
			// Fail open: if Redis is down, behave like a plain proxy.
			s.log.Warn("cache unavailable, bypassing", "err", err)
			s.bypass(w, r, path, body, start)
			return
		}
		key := k.String()
		w.Header().Set("X-Cache-Key", k.Hash[:12])

		if d.read {
			e, found, err := s.store.Get(r.Context(), key)
			if err != nil {
				s.log.Warn("cache read failed", "err", err)
			}
			if found {
				s.metrics.Requests.WithLabelValues("hit").Inc()
				age := max(0, time.Now().Unix()-e.StoredAt)
				w.Header().Set("X-Cache-Age", strconv.FormatInt(age, 10))
				s.write(w, kind, info.Stream, "HIT", &result{
					status: e.Status, contentType: e.ContentType, body: []byte(e.Body), ok: true,
				})
				s.metrics.OverheadSeconds.Observe(time.Since(start).Seconds())
				return
			}
		}

		// Miss: one goroutine per key calls upstream; the rest wait for it.
		// singleflight runs fn in the first caller's goroutine, so only the
		// leader's closure sets leader=true.
		leader := false
		s.metrics.InflightCoalesced.Inc()
		v, _, _ := s.group.Do(key, func() (any, error) {
			leader = true
			s.metrics.InflightCoalesced.Dec()
			return s.fetch(w, r, kind, path, body, info, key, d.write), nil
		})
		res := v.(*result)

		if !leader {
			s.metrics.InflightCoalesced.Dec()
			s.metrics.Requests.WithLabelValues("coalesced").Inc()
			s.write(w, kind, info.Stream, "COALESCED", res)
			return
		}
		s.metrics.Requests.WithLabelValues("miss").Inc()
		if !res.delivered {
			s.write(w, kind, info.Stream, "MISS", res)
		}
		s.metrics.OverheadSeconds.Observe((time.Since(start) - res.upstream).Seconds())
	}
}

func (s *Server) buildKey(r *http.Request, kind string, body []byte, model string) (keys.Key, error) {
	canon, err := keys.Canonical(kind, body)
	if err != nil {
		return keys.Key{}, err
	}
	tmpl := strings.TrimSpace(r.Header.Get("X-Prompt-Template"))
	gen, ver, err := s.store.Versions(r.Context(), model, tmpl)
	if err != nil {
		return keys.Key{}, err
	}
	return keys.Key{
		Tenant:      keys.Tenant(credential(r)),
		Model:       model,
		ModelGen:    gen,
		Template:    tmpl,
		TemplateVer: ver,
		Hash:        keys.Hash(canon),
	}, nil
}

// fetch runs only in the leader. It calls upstream, streams to the leader's
// client if it asked for a stream, and stores the response if allowed.
func (s *Server) fetch(w http.ResponseWriter, r *http.Request, kind, path string, body []byte, info reqInfo, key string, write bool) *result {
	// Detach from the leader's client: if it disconnects, waiters still need
	// the result and we still want to cache it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.UpstreamTimeout)
	defer cancel()

	start := time.Now()
	resp, err := s.upstream(ctx, r, path, body)
	if err != nil {
		s.metrics.UpstreamRequests.WithLabelValues("error").Inc()
		return &result{err: err, upstream: time.Since(start)}
	}
	defer resp.Body.Close()
	s.metrics.UpstreamRequests.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	var res *result
	if info.Stream && resp.StatusCode/100 == 2 &&
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		res = s.teeStream(w, resp)
	} else {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return &result{err: err, upstream: time.Since(start)}
		}
		res = &result{
			status:      resp.StatusCode,
			contentType: resp.Header.Get("Content-Type"),
			body:        b,
			ok:          resp.StatusCode/100 == 2,
		}
	}
	res.upstream = time.Since(start)
	s.metrics.UpstreamSeconds.Observe(res.upstream.Seconds())

	if write && s.storable(kind, res) {
		e := &cache.Entry{
			Status:      res.status,
			ContentType: "application/json",
			Body:        json.RawMessage(res.body),
			StoredAt:    time.Now().Unix(),
		}
		if err := s.store.Set(ctx, key, info.Model, e); err != nil {
			s.log.Warn("cache write failed", "err", err)
		} else {
			s.metrics.EntryBytes.Observe(float64(len(res.body)))
		}
	}
	return res
}

func (s *Server) storable(kind string, res *result) bool {
	if !res.ok || len(res.body) == 0 || len(res.body) > s.cfg.MaxEntryBytes || !json.Valid(res.body) {
		return false
	}
	return kind != keys.KindChat || finishReasonsOK(res.body)
}

func (s *Server) upstream(ctx context.Context, r *http.Request, path string, body []byte) (*http.Response, error) {
	u := *s.cfg.UpstreamURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range forwardHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(req)
}

// teeStream forwards SSE lines to the client as they arrive and assembles
// them into a chat.completion. The result is ok only if the stream reached
// [DONE] and every chunk parsed.
func (s *Server) teeStream(w http.ResponseWriter, resp *http.Response) *result {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)

	asm := newAssembler()
	rd := bufio.NewReader(resp.Body)
	clientOK, done, parsed := true, false, true
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			if clientOK {
				if _, werr := w.Write(line); werr != nil {
					clientOK = false // keep reading so the response can still be cached
				} else if fl != nil {
					fl.Flush()
				}
			}
			if p, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
				p = bytes.TrimSpace(p)
				if string(p) == "[DONE]" {
					done = true
				} else if !done {
					if err := asm.add(p); err != nil {
						parsed = false
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	res := &result{status: resp.StatusCode, delivered: true}
	if done && parsed {
		if b, err := asm.result(); err == nil {
			res.body, res.contentType, res.ok = b, "application/json", true
		}
	}
	return res
}

// write sends a result to a client that did not receive it live.
func (s *Server) write(w http.ResponseWriter, kind string, wantStream bool, cacheStatus string, res *result) {
	w.Header().Set("X-Cache", cacheStatus)
	switch {
	case res.err != nil:
		writeError(w, http.StatusBadGateway, "upstream request failed: "+res.err.Error())
	case len(res.body) == 0:
		writeError(w, http.StatusBadGateway, "upstream stream ended before completion")
	case res.ok && wantStream && kind == keys.KindChat:
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if err := writeSSE(w, res.body); err != nil {
			s.log.Warn("stream replay failed", "err", err)
		}
	default:
		ct := res.contentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(res.status)
		_, _ = w.Write(res.body)
	}
}

// bypass forwards the request without touching the cache.
func (s *Server) bypass(w http.ResponseWriter, r *http.Request, path string, body []byte, start time.Time) {
	s.metrics.Requests.WithLabelValues("bypass").Inc()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()

	t0 := time.Now()
	resp, err := s.upstream(ctx, r, path, body)
	if err != nil {
		s.metrics.UpstreamRequests.WithLabelValues("error").Inc()
		w.Header().Set("X-Cache", "BYPASS")
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	s.metrics.UpstreamRequests.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Cache", "BYPASS")
	w.WriteHeader(resp.StatusCode)
	copyFlush(w, resp.Body)

	up := time.Since(t0)
	s.metrics.UpstreamSeconds.Observe(up.Seconds())
	s.metrics.OverheadSeconds.Observe((time.Since(start) - up).Seconds())
}

func copyFlush(w http.ResponseWriter, src io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
