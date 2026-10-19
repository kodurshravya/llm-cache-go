package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssembleToolCallsAndReplayRoundTrip(t *testing.T) {
	chunks := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Jacksonville\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
	}
	a := newAssembler()
	for _, c := range chunks {
		if err := a.add([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	body, err := a.result()
	if err != nil {
		t.Fatal(err)
	}
	var got completion
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tc := got.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "get_weather" ||
		tc[0].Function.Arguments != `{"city":"Jacksonville"}` {
		t.Fatalf("tool calls: %+v", tc)
	}
	if *got.Choices[0].FinishReason != "tool_calls" || !hasUsage(got.Usage) {
		t.Fatalf("finish/usage: %s", body)
	}

	// Replay, then reassemble: must equal what we stored.
	rec := httptest.NewRecorder()
	if err := writeSSE(rec, body); err != nil {
		t.Fatal(err)
	}
	b := newAssembler()
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		p, ok := strings.CutPrefix(line, "data: ")
		if !ok || p == "[DONE]" {
			continue
		}
		if err := b.add([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := b.result()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(body) {
		t.Fatalf("round trip differs:\n%s\n%s", body, again)
	}
	if !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatal("replay must end with [DONE]")
	}
}

func TestDecide(t *testing.T) {
	zero, warm, two := 0.0, 0.7, 2.0
	cases := []struct {
		name        string
		kind        string
		info        reqInfo
		cc          string
		read, write bool
	}{
		{"temp 0", "chat", reqInfo{Temperature: &zero}, "", true, true},
		{"temp missing", "chat", reqInfo{}, "", false, false},
		{"temp 0.7", "chat", reqInfo{Temperature: &warm}, "", false, false},
		{"force", "chat", reqInfo{Temperature: &warm}, "force", true, true},
		{"embeddings", "embeddings", reqInfo{}, "", true, true},
		{"no-cache", "chat", reqInfo{Temperature: &zero}, "no-cache", false, true},
		{"no-store", "chat", reqInfo{Temperature: &zero}, "no-store", false, false},
		{"n=2", "chat", reqInfo{Temperature: &zero, N: &two}, "", false, false},
		{"force, no-cache", "chat", reqInfo{Temperature: &warm}, "force, No-Cache", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.kind, tc.info, tc.cc)
			if d.read != tc.read || d.write != tc.write {
				t.Fatalf("got read=%v write=%v", d.read, d.write)
			}
		})
	}
}
