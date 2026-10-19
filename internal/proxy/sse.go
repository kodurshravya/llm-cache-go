package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Types for a non-streamed chat completion (the stored form).

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type message struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	Refusal   *string    `json:"refusal,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type completion struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	Choices           []choice        `json:"choices"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

// Types for streamed chunks.

type toolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Refusal   *string         `json:"refusal,omitempty"`
	ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunk struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	Choices           []chunkChoice   `json:"choices"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

func hasUsage(u json.RawMessage) bool { return len(u) > 0 && string(u) != "null" }

// assembler rebuilds a chat.completion from chat.completion.chunk events.
type assembler struct {
	c       completion
	choices map[int]*choiceAcc
	order   []int
}

type choiceAcc struct {
	role       string
	content    strings.Builder
	hasContent bool
	refusal    strings.Builder
	hasRefusal bool
	tools      map[int]*toolCall
	toolOrder  []int
	finish     *string
}

func newAssembler() *assembler {
	return &assembler{choices: map[int]*choiceAcc{}}
}

func (a *assembler) choice(i int) *choiceAcc {
	acc, ok := a.choices[i]
	if !ok {
		acc = &choiceAcc{tools: map[int]*toolCall{}}
		a.choices[i] = acc
		a.order = append(a.order, i)
	}
	return acc
}

func (acc *choiceAcc) tool(i int) *toolCall {
	tc, ok := acc.tools[i]
	if !ok {
		tc = &toolCall{}
		acc.tools[i] = tc
		acc.toolOrder = append(acc.toolOrder, i)
	}
	return tc
}

// add consumes one SSE data payload (without the "data:" prefix).
func (a *assembler) add(payload []byte) error {
	var ch chunk
	if err := json.Unmarshal(payload, &ch); err != nil {
		return err
	}
	if a.c.ID == "" {
		a.c.ID = ch.ID
	}
	if a.c.Created == 0 {
		a.c.Created = ch.Created
	}
	if a.c.Model == "" {
		a.c.Model = ch.Model
	}
	if ch.SystemFingerprint != "" {
		a.c.SystemFingerprint = ch.SystemFingerprint
	}
	if hasUsage(ch.Usage) {
		a.c.Usage = ch.Usage
	}
	for _, cc := range ch.Choices {
		acc := a.choice(cc.Index)
		if cc.Delta.Role != "" {
			acc.role = cc.Delta.Role
		}
		if cc.Delta.Content != nil {
			acc.hasContent = true
			acc.content.WriteString(*cc.Delta.Content)
		}
		if cc.Delta.Refusal != nil {
			acc.hasRefusal = true
			acc.refusal.WriteString(*cc.Delta.Refusal)
		}
		for _, td := range cc.Delta.ToolCalls {
			tc := acc.tool(td.Index)
			if td.ID != "" {
				tc.ID = td.ID
			}
			if td.Type != "" {
				tc.Type = td.Type
			}
			tc.Function.Name += td.Function.Name
			tc.Function.Arguments += td.Function.Arguments
		}
		if cc.FinishReason != nil {
			f := *cc.FinishReason
			acc.finish = &f
		}
	}
	return nil
}

// result returns the assembled chat.completion JSON.
func (a *assembler) result() ([]byte, error) {
	out := a.c
	out.Object = "chat.completion"
	out.Choices = make([]choice, 0, len(a.order))
	sort.Ints(a.order)
	for _, idx := range a.order {
		acc := a.choices[idx]
		m := message{Role: acc.role}
		if m.Role == "" {
			m.Role = "assistant"
		}
		if acc.hasContent {
			s := acc.content.String()
			m.Content = &s
		}
		if acc.hasRefusal {
			s := acc.refusal.String()
			m.Refusal = &s
		}
		sort.Ints(acc.toolOrder)
		for _, ti := range acc.toolOrder {
			m.ToolCalls = append(m.ToolCalls, *acc.tools[ti])
		}
		out.Choices = append(out.Choices, choice{Index: idx, Message: m, FinishReason: acc.finish})
	}
	return json.Marshal(out)
}

// writeSSE replays a stored chat.completion as chat.completion.chunk events
// followed by "data: [DONE]". The caller sets headers and the status first.
func writeSSE(w http.ResponseWriter, body []byte) error {
	var c completion
	if err := json.Unmarshal(body, &c); err != nil {
		return err
	}
	fl, _ := w.(http.Flusher)
	emit := func(ch chunk) error {
		b, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	base := chunk{
		ID: c.ID, Object: "chat.completion.chunk", Created: c.Created,
		Model: c.Model, SystemFingerprint: c.SystemFingerprint,
	}
	for _, ch := range c.Choices {
		d := chunkDelta{Role: ch.Message.Role, Content: ch.Message.Content, Refusal: ch.Message.Refusal}
		for i, tc := range ch.Message.ToolCalls {
			d.ToolCalls = append(d.ToolCalls, toolCallDelta{Index: i, ID: tc.ID, Type: tc.Type, Function: tc.Function})
		}
		first := base
		first.Choices = []chunkChoice{{Index: ch.Index, Delta: d}}
		if err := emit(first); err != nil {
			return err
		}
		last := base
		last.Choices = []chunkChoice{{Index: ch.Index, FinishReason: ch.FinishReason}}
		if err := emit(last); err != nil {
			return err
		}
	}
	if hasUsage(c.Usage) {
		u := base
		u.Choices = []chunkChoice{}
		u.Usage = c.Usage
		if err := emit(u); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}
