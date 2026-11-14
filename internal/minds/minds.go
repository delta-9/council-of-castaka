// Package minds puts each provider behind one call: speak a turn, calling
// tools on the way.
package minds

import (
	"context"
	"encoding/json"
	"fmt"
)

// Turn is one message of a mind's view of the floor. An assistant turn may
// carry Raw: the provider's native exchange for that turn (tool calls, tool
// results, reasoning), replayed as received so signatures, the history check
// and prompt caches survive from one turn to the next.
type Turn struct {
	Role  string // "user" or "assistant"
	Text  string
	Raw   json.RawMessage
	Model string // assistant turns: the model the seat was configured with
}

// Usage is normalized across providers; Input excludes cache reads and writes.
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	Output     int64 `json:"output"`
	Thinking   int64 `json:"thinking,omitempty"`
	Searches   int64 `json:"searches,omitempty"` // web searches the provider ran, billed per search
}

func (u *Usage) add(v Usage) {
	u.Input += v.Input
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.Output += v.Output
	u.Thinking += v.Thinking
	u.Searches += v.Searches
}

// Reply is what a mind said, with what is needed to replay and bill it.
type Reply struct {
	Text   string
	Raw    json.RawMessage
	Served string // the model that answered, as the provider reports it
	Stop   string // the provider's stop or finish reason
	Usage  Usage
	Calls  []string // the tools called this turn, described for the chair
}

// Mind speaks one turn, given its system prompt and its view of the floor. On
// an error, the reply still carries the usage and calls of the steps that ran.
type Mind interface {
	Speak(ctx context.Context, system string, turns []Turn) (Reply, error)
}

// Tool is a local tool a mind can call.
type Tool struct {
	Name        string
	Description string
	Properties  map[string]any // a JSON schema per parameter
	Required    []string
}

// Schema returns the tool's input as a JSON schema object.
func (t Tool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": t.Properties, "required": t.Required}
}

// Toolbox runs a seat's local tools.
type Toolbox interface {
	Tools() []Tool
	Run(ctx context.Context, name string, input json.RawMessage) (string, error)
	Describe(name string, input json.RawMessage) string
}

// Spec is one seat of a roster in council.json.
type Spec struct {
	Seat     string `json:"seat"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Search   string `json:"search,omitempty"` // "native" (the default), "mcp" or "off"
	Card     string `json:"card,omitempty"`
	// MaxTokens overrides the session's output budget for this seat; a model
	// that thinks long at high effort (Carbon) needs more, since its thinking
	// and its answer share the budget.
	MaxTokens int64 `json:"maxTokens,omitempty"`
}

// NativeSearch reports whether the seat searches with its provider's own tool.
func (s Spec) NativeSearch() bool {
	return s.Search == "" || s.Search == "native"
}

// Options apply to every mind of a session.
type Options struct {
	MaxTokens int64
	MaxCalls  int          // local tool calls per turn before the mind must answer; 0 means DefaultMaxCalls
	Session   string       // keys OpenAI's prompt cache per session and seat
	Tools     Toolbox      // the seat's local tools; never nil
	Notify    func(string) // reports each tool call as it happens; may be nil
}

const (
	// DefaultMaxCalls is how many local tools a mind may call in one turn before it must answer.
	DefaultMaxCalls = 25
	// maxSteps bounds the requests of one turn, server-side pauses included.
	maxSteps = 40
	// nudge asks once for the answer when a turn ends cleanly with no text.
	nudge = "Your reply came back empty. Answer now, in text, with what you have."
)

func (o Options) maxCalls() int {
	if o.MaxCalls > 0 {
		return o.MaxCalls
	}
	return DefaultMaxCalls
}

// spent marks the result of a call that used up the turn's tool budget. Told
// nothing, Fable once announced one more check it could no longer run and
// ended its turn without an answer.
func spent(out string, calls int, opt Options) string {
	if calls < opt.maxCalls() {
		return out
	}
	return out + fmt.Sprintf("\n\n[That was this turn's last tool call (%d): tools are off now. Answer with what you have.]", opt.maxCalls())
}

// New builds the mind for spec.
func New(spec Spec, key string, opt Options) (Mind, error) {
	switch spec.Provider {
	case "anthropic":
		return newAnthropic(spec, key, opt), nil
	case "gemini":
		return newGemini(spec, key, opt)
	case "openai":
		return newOpenAI(spec, key, opt), nil
	case "fake":
		return fake{seat: spec.Seat, opt: opt}, nil
	}
	return nil, fmt.Errorf("seat %s: unknown provider %q", spec.Seat, spec.Provider)
}

// Lab names a provider for the charter's roster.
func Lab(provider string) string {
	switch provider {
	case "anthropic":
		return "Anthropic"
	case "gemini":
		return "Google"
	case "openai":
		return "OpenAI"
	}
	return provider
}

// call runs one local tool for a turn and reports it. A failure goes back to
// the mind as text, with failed set.
func call(ctx context.Context, opt Options, reply *Reply, name string, input json.RawMessage) (out string, failed bool) {
	note(opt, reply, opt.Tools.Describe(name, input))
	out, err := opt.Tools.Run(ctx, name, input)
	if err != nil {
		return "error: " + err.Error(), true
	}
	return out, false
}

// searched reports a web search the provider ran itself.
func searched(opt Options, reply *Reply, query string) {
	input, _ := json.Marshal(map[string]string{"query": query})
	note(opt, reply, opt.Tools.Describe("web_search", input))
}

func note(opt Options, reply *Reply, call string) {
	reply.Calls = append(reply.Calls, call)
	if opt.Notify != nil {
		opt.Notify(call)
	}
}
