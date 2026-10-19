// Command fakeupstream is a deterministic OpenAI-style server with a fixed
// delay, so load tests measure the proxy rather than a real model.
//
// The reply is "echo: <last message>". max_tokens=1 returns finish_reason
// "length" (useful for checking that truncated answers aren't cached).
// GET /stats returns the number of upstream calls; POST /stats/reset zeroes it.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var calls atomic.Int64

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	latency := flag.Duration("latency", 300*time.Millisecond, "fixed delay before each response")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
			return
		}
		if !sleep(r, *latency) {
			return
		}
		last := ""
		if n := len(req.Messages); n > 0 {
			last = fmt.Sprint(req.Messages[n-1].Content)
		}
		content := "echo: " + last
		finish := "stop"
		if req.MaxTokens == 1 {
			finish = "length"
		}
		sum := sha256.Sum256([]byte(req.Model + last))
		id := "chatcmpl-fake-" + hex.EncodeToString(sum[:6])
		created := time.Now().Unix()

		if !req.Stream {
			writeJSON(w, map[string]any{
				"id": id, "object": "chat.completion", "created": created, "model": req.Model,
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": finish,
				}},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(delta map[string]any, fin any) {
			b, _ := json.Marshal(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
			})
			_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		send(map[string]any{"role": "assistant", "content": ""}, nil)
		for _, word := range strings.SplitAfter(content, " ") {
			send(map[string]any{"content": word}, nil)
		}
		send(map[string]any{}, finish)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model string `json:"model"`
			Input any    `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
			return
		}
		if !sleep(r, *latency) {
			return
		}
		inputs := []any{req.Input}
		if list, ok := req.Input.([]any); ok {
			inputs = list
		}
		data := make([]any, 0, len(inputs))
		for i, in := range inputs {
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector(fmt.Sprint(in))})
		}
		writeJSON(w, map[string]any{"object": "list", "data": data, "model": req.Model,
			"usage": map[string]int{"prompt_tokens": 5, "total_tokens": 5}})
	})

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]int64{"calls": calls.Load()})
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, _ *http.Request) {
		calls.Store(0)
		writeJSON(w, map[string]int64{"calls": 0})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "fakeupstream ok"})
	})

	log.Printf("fakeupstream listening on %s with latency %s", *addr, *latency)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// sleep waits d, returning false if the client went away first.
func sleep(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

// vector is a deterministic 8-dimensional embedding derived from the input.
func vector(s string) []float64 {
	sum := sha256.Sum256([]byte(s))
	out := make([]float64, 8)
	for i := range out {
		out[i] = float64(binary.BigEndian.Uint32(sum[i*4:])) / float64(1<<32)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
