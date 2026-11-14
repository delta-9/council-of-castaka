package minds

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

type anthropicMind struct {
	client anthropic.Client
	spec   Spec
	opt    Options
}

func newAnthropic(spec Spec, key string, opt Options) Mind {
	client := anthropic.NewClient(option.WithAPIKey(key), option.WithRequestTimeout(15*time.Minute))
	return &anthropicMind{client: client, spec: spec, opt: opt}
}

// Speak sends the turn with the seat's effort, automatic prompt caching and
// server-side fallbacks for refusals. Thinking stays unset: it is always on
// for Opus 5.5 and Fable 5.2, and effort sets its depth.
func (m *anthropicMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	msgs := make([]anthropic.BetaMessageParam, 0, len(turns))
	for _, t := range turns {
		if t.Role == "user" {
			msgs = append(msgs, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(t.Text)))
		} else {
			msgs = append(msgs, anthropicReplay(t))
		}
	}
	resp, err := m.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:        anthropic.Model(m.spec.Model),
		MaxTokens:    m.opt.MaxTokens,
		System:       []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:     msgs,
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(m.spec.Effort)},
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		return Reply{}, err
	}
	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	reply := Reply{
		Text:   text.String(),
		Raw:    json.RawMessage(resp.RawJSON()),
		Served: string(resp.Model),
		Stop:   string(resp.StopReason),
		Usage: Usage{
			Input:      resp.Usage.InputTokens,
			CacheRead:  resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens,
			Output:     resp.Usage.OutputTokens,
		},
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		reply.Stop = "refusal " + string(resp.StopDetails.Category)
	}
	return reply, nil
}

// anthropicReplay sends a past turn back as the API returned it, thinking
// blocks and signatures included: unchanged history keeps them valid under the
// history-editing check. After a mid-output fallback, the reasoning blocks
// before the last fallback block are left out, as the fallback rules require.
func anthropicReplay(t Turn) anthropic.BetaMessageParam {
	var msg anthropic.BetaMessage
	if len(t.Raw) == 0 || json.Unmarshal(t.Raw, &msg) != nil {
		return anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(t.Text)},
		}}
	}
	last := -1
	for i, b := range msg.Content {
		if b.Type == "fallback" {
			last = i
		}
	}
	kept := make([]anthropic.BetaContentBlockUnion, 0, len(msg.Content))
	for i, b := range msg.Content {
		if i < last && (b.Type == "thinking" || b.Type == "redacted_thinking" || b.Type == "tool_use") {
			continue
		}
		kept = append(kept, b)
	}
	msg.Content = kept
	return msg.ToParam()
}
