package proxy

import (
	"encoding/json"
	"strings"

	"github.com/kodurshravya/llm-cache-go/internal/keys"
)

// reqInfo is the small part of the request the policy needs.
type reqInfo struct {
	Model       string   `json:"model"`
	Stream      bool     `json:"stream"`
	Temperature *float64 `json:"temperature"`
	N           *float64 `json:"n"`
}

// decision says whether to read from and/or write to the cache.
// Both false means bypass.
type decision struct {
	read, write bool
	reason      string
}

// decide applies the README's cache policy. X-Cache-Control may hold
// several comma-separated values, e.g. "force, no-cache".
func decide(kind string, info reqInfo, cacheControl string) decision {
	cc := map[string]bool{}
	for _, v := range strings.Split(cacheControl, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			cc[v] = true
		}
	}
	if cc["no-store"] {
		return decision{reason: "no-store"}
	}
	if info.N != nil && *info.N > 1 {
		return decision{reason: "n>1"}
	}
	cacheable := kind == keys.KindEmbeddings ||
		cc["force"] ||
		(info.Temperature != nil && *info.Temperature == 0)
	if !cacheable {
		// OpenAI's default temperature is 1, so a missing value is not cacheable.
		return decision{reason: "nondeterministic"}
	}
	if cc["no-cache"] {
		return decision{write: true, reason: "no-cache"}
	}
	return decision{read: true, write: true}
}

// finishReasonsOK is false if any choice stopped because of length or the
// content filter: those responses are incomplete and must not be cached.
func finishReasonsOK(body []byte) bool {
	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return false
	}
	for _, ch := range c.Choices {
		if ch.FinishReason == "length" || ch.FinishReason == "content_filter" {
			return false
		}
	}
	return true
}
