package minds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
)

type anthropicMind struct {
	client anthropic.Client
	spec   Spec
	opt    Options
	tools  []anthropic.BetaToolUnionParam
}

func newAnthropic(spec Spec, key string, opt Options) Mind {
	m := &anthropicMind{
		client: anthropic.NewClient(option.WithAPIKey(key), option.WithRequestTimeout(15*time.Minute)),
		spec:   spec,
		opt:    opt,
	}
	if spec.NativeSearch() {
		m.tools = append(m.tools, anthropic.BetaToolUnionParam{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{MaxUses: anthropic.Int(5)}})
	}
	for _, t := range opt.Tools.Tools() {
		m.tools = append(m.tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: t.Properties, Required: t.Required},
		}})
	}
	return m
}

// Speak runs the turn with the seat's effort, automatic prompt caching,
// server-side fallbacks for refusals and the seat's tools, looping on tool
// calls until the mind answers. Thinking stays unset: it is always on for
// Opus 5.5 and Fable 5.2, and effort sets its depth. The tool list never
// changes within a session, since Fable's history check compares it.
func (m *anthropicMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	var history []anthropic.BetaMessageParam
	for _, t := range turns {
		if t.Role == "user" {
			history = append(history, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(t.Text)))
		} else {
			history = append(history, anthropicReplay(t)...)
		}
	}
	var reply Reply
	var exchange []anthropic.BetaMessageParam
	calls, nudged := 0, false
	for step := 0; step < maxSteps; step++ {
		params := anthropic.BetaMessageNewParams{
			Model:        anthropic.Model(m.spec.Model),
			MaxTokens:    m.opt.MaxTokens,
			System:       []anthropic.BetaTextBlockParam{{Text: system}},
			Messages:     append(append([]anthropic.BetaMessageParam(nil), history...), exchange...),
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(m.spec.Effort)},
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
			Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Tools:        m.tools,
		}
		if calls >= m.opt.maxCalls() {
			params.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
		}
		resp, err := m.client.Beta.Messages.New(ctx, params)
		if err != nil {
			return reply, err
		}
		reply.Served = string(resp.Model)
		reply.Usage.add(Usage{
			Input:      resp.Usage.InputTokens,
			CacheRead:  resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens,
			Output:     resp.Usage.OutputTokens,
			Searches:   resp.Usage.ServerToolUse.WebSearchRequests,
		})
		for _, b := range resp.Content {
			if b.Type == "server_tool_use" && b.Name == "web_search" {
				var in struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal(b.Input, &in)
				searched(m.opt, &reply, in.Query)
			}
		}
		exchange = append(exchange, anthropicParam(*resp))
		if resp.StopReason == anthropic.BetaStopReasonPauseTurn {
			continue // the server resumes its search loop from the trailing block
		}
		if resp.StopReason != anthropic.BetaStopReasonToolUse {
			var text strings.Builder
			for _, b := range resp.Content {
				if b.Type == "text" {
					text.WriteString(b.Text)
				}
			}
			if strings.TrimSpace(text.String()) == "" && resp.StopReason == anthropic.BetaStopReasonEndTurn && !nudged {
				nudged = true
				exchange = append(exchange, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(nudge)))
				continue
			}
			reply.Text, reply.Stop = text.String(), string(resp.StopReason)
			if resp.StopReason == anthropic.BetaStopReasonRefusal {
				reply.Stop = "refusal " + string(resp.StopDetails.Category)
			}
			reply.Raw, err = json.Marshal(exchange)
			return reply, err
		}
		var results []anthropic.BetaContentBlockParamUnion
		for _, b := range resp.Content {
			if b.Type == "tool_use" {
				calls++
				out, failed := call(ctx, m.opt, &reply, b.Name, b.Input)
				results = append(results, anthropic.NewBetaToolResultBlock(b.ID, spent(out, calls, m.opt), failed))
			}
		}
		exchange = append(exchange, anthropic.NewBetaUserMessage(results...))
	}
	return reply, errors.New("the turn ran past its request limit")
}

// anthropicParam turns a response into the assistant message that replays it.
// After a mid-output fallback, the reasoning blocks before the last fallback
// block are left out, as the fallback rules require.
func anthropicParam(msg anthropic.BetaMessage) anthropic.BetaMessageParam {
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

// anthropicReplay sends a past turn back as it ran: every message of its
// exchange, thinking blocks and signatures included, so they stay valid under
// the history-editing check. A turn logged before tools existed is one
// response; a turn with no native reply goes back as text.
func anthropicReplay(t Turn) []anthropic.BetaMessageParam {
	var exchange []json.RawMessage
	if json.Unmarshal(t.Raw, &exchange) == nil && len(exchange) > 0 {
		msgs := make([]anthropic.BetaMessageParam, len(exchange))
		for i, raw := range exchange {
			msgs[i] = param.Override[anthropic.BetaMessageParam](raw)
		}
		return msgs
	}
	var msg anthropic.BetaMessage
	if len(t.Raw) > 0 && json.Unmarshal(t.Raw, &msg) == nil {
		return []anthropic.BetaMessageParam{anthropicParam(msg)}
	}
	return []anthropic.BetaMessageParam{{OfAssistant: &anthropic.BetaAssistantMessageParam{
		Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(t.Text)},
	}}}
}
