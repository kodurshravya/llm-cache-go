package keys

import (
	"strings"
	"testing"
)

const base = `{"model":"gpt-4.1-mini","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

func hashOf(t *testing.T, kind, body string) string {
	t.Helper()
	c, err := Canonical(kind, []byte(body))
	if err != nil {
		t.Fatalf("Canonical(%s): %v", body, err)
	}
	return Hash(c)
}

func TestCanonicalSameKey(t *testing.T) {
	want := hashOf(t, KindChat, base)
	cases := map[string]string{
		"reordered fields":   `{"messages":[{"content":"hi","role":"user"}],"temperature":0,"model":"gpt-4.1-mini"}`,
		"extra whitespace":   "{ \"model\" : \"gpt-4.1-mini\",\n \"temperature\": 0,\n \"messages\": [ {\"role\":\"user\", \"content\":\"hi\"} ] }",
		"stream excluded":    `{"model":"gpt-4.1-mini","temperature":0,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"user excluded":      `{"model":"gpt-4.1-mini","temperature":0,"user":"abc","messages":[{"role":"user","content":"hi"}]}`,
		"0.0 equals 0":       `{"model":"gpt-4.1-mini","temperature":0.0,"messages":[{"role":"user","content":"hi"}]}`,
		"stream_options out": `{"model":"gpt-4.1-mini","temperature":0,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hashOf(t, KindChat, body); got != want {
				t.Errorf("hash differs from base")
			}
		})
	}
}

func TestCanonicalDifferentKey(t *testing.T) {
	baseHash := hashOf(t, KindChat, base)
	cases := map[string]string{
		"temperature":  `{"model":"gpt-4.1-mini","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`,
		"message":      `{"model":"gpt-4.1-mini","temperature":0,"messages":[{"role":"user","content":"hello"}]}`,
		"model":        `{"model":"gpt-4.1","temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
		"tool added":   `{"model":"gpt-4.1-mini","temperature":0,"tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`,
		"max_tokens":   `{"model":"gpt-4.1-mini","temperature":0,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`,
		"seed":         `{"model":"gpt-4.1-mini","temperature":0,"seed":7,"messages":[{"role":"user","content":"hi"}]}`,
		"message role": `{"model":"gpt-4.1-mini","temperature":0,"messages":[{"role":"system","content":"hi"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hashOf(t, KindChat, body); got == baseHash {
				t.Errorf("expected a different hash")
			}
		})
	}
}

func TestKindIsPartOfKey(t *testing.T) {
	body := `{"model":"m","input":"x"}`
	if hashOf(t, KindChat, body) == hashOf(t, KindEmbeddings, body) {
		t.Fatal("chat and embeddings requests must not share a key")
	}
}

func TestCanonicalRejectsBadJSON(t *testing.T) {
	if _, err := Canonical(KindChat, []byte(`{not json`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTenant(t *testing.T) {
	a, b := Tenant("Bearer key-a"), Tenant("Bearer key-b")
	if len(a) != 16 || a == b {
		t.Fatalf("tenants: %q %q", a, b)
	}
	if strings.Contains(a, "key-a") {
		t.Fatal("raw credential leaked into tenant")
	}
}

func TestKeyString(t *testing.T) {
	k := Key{Tenant: "t", Model: "m", ModelGen: 2, TemplateVer: 0, Hash: "h"}
	if got, want := k.String(), "llmc:t:m:g2:-@0:h"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	k.Template, k.TemplateVer = "invoice-audit-v2", 3
	if got, want := k.String(), "llmc:t:m:g2:invoice-audit-v2@3:h"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
