// Package keys turns an OpenAI-style request into a stable cache key.
//
// Two requests that mean the same thing (same fields, different order or
// whitespace) produce the same key. Fields that do not change the model's
// output, such as "user" or "stream", are left out.
package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Request kinds. Each kind has its own list of output-affecting fields.
const (
	KindChat       = "chat"
	KindEmbeddings = "embeddings"
)

var fieldsByKind = map[string][]string{
	KindChat: {
		"model", "messages", "tools", "tool_choice", "parallel_tool_calls",
		"response_format", "temperature", "top_p", "max_tokens",
		"max_completion_tokens", "seed", "stop", "frequency_penalty",
		"presence_penalty", "logit_bias", "logprobs", "top_logprobs",
		"reasoning_effort",
	},
	KindEmbeddings: {"model", "input", "dimensions", "encoding_format"},
}

// Canonical returns the request re-serialized with sorted object keys, no
// insignificant whitespace, normalized numbers, and only the fields that
// change the output.
func Canonical(kind string, body []byte) ([]byte, error) {
	fields, ok := fieldsByKind[kind]
	if !ok {
		return nil, fmt.Errorf("keys: unknown kind %q", kind)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("keys: decode request: %w", err)
	}
	out := make(map[string]any, len(fields)+1)
	out["_kind"] = kind
	for _, f := range fields {
		if v, ok := req[f]; ok {
			out[f] = normalize(v)
		}
	}
	// encoding/json sorts map keys and emits compact output.
	return json.Marshal(out)
}

// normalize makes 0, 0.0 and 0e0 serialize the same way.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalize(val)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}

// Hash is the hex SHA-256 of a canonical request.
func Hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Tenant is the first 16 hex chars of SHA-256(credential). The raw
// credential is never stored or logged.
func Tenant(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])[:16]
}

// Key holds every part of a cache key.
type Key struct {
	Tenant      string
	Model       string
	ModelGen    int64
	Template    string
	TemplateVer int64
	Hash        string
}

// String renders llmc:{tenant}:{model}:g{modelGen}:{template}@{ver}:{hash}.
func (k Key) String() string {
	tmpl := k.Template
	if tmpl == "" {
		tmpl = "-"
	}
	return fmt.Sprintf("llmc:%s:%s:g%d:%s@%d:%s", k.Tenant, k.Model, k.ModelGen, tmpl, k.TemplateVer, k.Hash)
}
