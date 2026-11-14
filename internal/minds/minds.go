// Package minds puts each provider behind one call: speak a turn.
package minds

import (
	"context"
	"encoding/json"
	"fmt"
)

// Turn is one message of a mind's view of the floor. An assistant turn may
// carry Raw, the provider's native reply, replayed as received so reasoning
// blocks, signatures and prompt caches survive from one turn to the next.
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
}

// Reply is what a mind said, with what is needed to replay and bill it.
type Reply struct {
	Text   string
	Raw    json.RawMessage
	Served string // the model that answered, as the provider reports it
	Stop   string // the provider's stop or finish reason
	Usage  Usage
}

// Mind speaks one turn, given its system prompt and its view of the floor.
type Mind interface {
	Speak(ctx context.Context, system string, turns []Turn) (Reply, error)
}

// Spec is one seat of a roster in council.json.
type Spec struct {
	Seat     string `json:"seat"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Card     string `json:"card,omitempty"`
}

// Options apply to every mind of a session.
type Options struct {
	MaxTokens int64
	Session   string // keys OpenAI's prompt cache per session and seat
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
		return fake{seat: spec.Seat}, nil
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
