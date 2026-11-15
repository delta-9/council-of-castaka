package minds

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
)

var (
	errTruncated   = errors.New("the stream ended before message_stop")
	errStepTimeout = errors.New("a request ran past its time limit")
	errTurnTimeout = errors.New("the turn ran past its time limit")
)

// limits bound an Anthropic turn. idle is the longest the connection may stay
// silent, pings included; step bounds one request with its SDK retries; turn
// bounds the whole loop, tool calls included; retry is the wait before a step
// that failed partway runs again.
type limits struct{ idle, step, turn, retry time.Duration }

var anthropicLimits = limits{idle: 120 * time.Second, step: 15 * time.Minute, turn: 30 * time.Minute, retry: 2 * time.Second}

type anthropicMind struct {
	client anthropic.Client
	spec   Spec
	opt    Options
	tools  []anthropic.BetaToolUnionParam
	limits limits
}

func newAnthropic(spec Spec, key string, opt Options) Mind {
	return anthropicWith(spec, key, opt, anthropicLimits)
}

// anthropicWith builds the mind with its limits and client options; tests
// shrink the limits and point the client at a fake API. The client has no
// request timeout: on a stream it would cut long healthy turns, and the step
// limit already bounds each request.
func anthropicWith(spec Spec, key string, opt Options, l limits, extra ...option.RequestOption) *anthropicMind {
	base := http.DefaultTransport.(*http.Transport).Clone()
	// HTTP/2 health checks: a connection silent for 45 s gets a ping, and one
	// that does not answer within 15 s is closed. A healthy stream is never
	// that quiet (the longest silence measured is 30 s), and a dead connection
	// fails its request and leaves the pool within a minute, so the SDK's
	// retry dials a fresh one instead of stalling on the same connection
	// until the watchdog.
	base.HTTP2 = &http.HTTP2Config{SendPingTimeout: 45 * time.Second, PingTimeout: 15 * time.Second}
	transport := watchdog{base: base, idle: l.idle}
	opts := append([]option.RequestOption{option.WithAPIKey(key), option.WithHTTPClient(&http.Client{Transport: transport})}, extra...)
	m := &anthropicMind{client: anthropic.NewClient(opts...), spec: spec, opt: opt, limits: l}
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
// calls until the mind answers. Each request is streamed (see step), and a
// step whose stream fails partway runs once more (see retryable).
// Thinking stays unset: it is always on for Opus 5.5 and Fable 5.2, and
// effort sets its depth. The tool list never changes within a session, since
// Fable's history check compares it.
func (m *anthropicMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.limits.turn, errTurnTimeout)
	defer cancel()
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
		resp, err := m.step(ctx, params, &reply)
		if why := retryable(err); why != "" {
			// One more try of this step costs less than the next round
			// rerunning every step of the turn, searches included.
			if m.opt.Notify != nil {
				m.opt.Notify("retrying after " + why)
			}
			if err = wait(ctx, m.limits.retry); err == nil {
				resp, err = m.step(ctx, params, &reply)
			}
		}
		if err != nil {
			return reply, err
		}
		reply.Served = string(resp.Model)
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

// step streams one request and builds it back into the message the rest of
// Speak expects. It fails unless the stream reached message_stop, so a cut
// stream is never stored as a turn, and it stops reading there, so whatever
// the connection does next cannot fail a finished answer. It adds the
// request's usage once, whether or not the request succeeded: input from
// message_start, output from the last message_delta, which a failed stream may
// never send. It notes the longest silence on the wire, and reports each web
// search when its block ends, once its query is complete. The served model
// comes from the accumulated message, which the SDK relabels at each fallback
// block.
func (m *anthropicMind) step(ctx context.Context, params anthropic.BetaMessageNewParams, reply *Reply) (*anthropic.BetaMessage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, m.limits.step, errStepTimeout)
	defer cancel()
	quiet := &silence{}
	stream := m.client.Beta.Messages.NewStreaming(context.WithValue(ctx, silenceKey{}, quiet), params)
	defer stream.Close()
	var msg anthropic.BetaMessage
	var err error
	stopped := false
	for err == nil && !stopped && stream.Next() {
		event := stream.Current()
		if err = msg.Accumulate(event); err != nil {
			break
		}
		switch event.Type {
		case "content_block_stop":
			if i := int(event.Index); i < len(msg.Content) && msg.Content[i].Type == "server_tool_use" && msg.Content[i].Name == "web_search" {
				var in struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal(msg.Content[i].Input, &in)
				searched(m.opt, reply, in.Query)
			}
		case "message_stop":
			stopped = true
		}
	}
	if err == nil && !stopped {
		if err = stream.Err(); err == nil {
			err = errTruncated
		}
	}
	reply.Quiet = max(reply.Quiet, quiet.max())
	if msg.ID != "" {
		reply.Usage.add(Usage{
			Input:      msg.Usage.InputTokens,
			CacheRead:  msg.Usage.CacheReadInputTokens,
			CacheWrite: msg.Usage.CacheCreationInputTokens,
			Output:     msg.Usage.OutputTokens,
			Searches:   msg.Usage.ServerToolUse.WebSearchRequests,
		})
	}
	if err != nil {
		// Say why: a time limit of the step or the turn rather than the error
		// the cut read produced. A stop by the chair stays a cancellation.
		if cause := context.Cause(ctx); cause == errStepTimeout || cause == errTurnTimeout || errors.Is(cause, context.Canceled) {
			return nil, cause
		}
		// A stream that started and then broke, rather than an error the API
		// sent: Speak may run the step once more.
		var apiErr *anthropic.Error
		if msg.ID != "" && !errors.As(err, &apiErr) {
			err = cutError{err}
		}
		return nil, err
	}
	return &msg, nil
}

// cutError is a stream that started and then broke: its connection lost,
// stalled or cut short.
type cutError struct{ err error }

func (e cutError) Error() string { return e.err.Error() }
func (e cutError) Unwrap() error { return e.err }

// retryable says why a step failed partway when the step is worth running
// once more, in place: the API overloaded or erring mid-stream, or a stream
// that started and then broke. Anything else gives "": an error the API sends
// for a bad request, the chair's stop, the time limits, and a failure before
// the stream started, which the SDK has already retried.
func retryable(err error) string {
	var apiErr *anthropic.Error
	var cut cutError
	switch {
	case errors.As(err, &apiErr):
		if t := apiErr.Type(); apiErr.StatusCode == http.StatusOK && (t == anthropic.ErrorTypeOverloadedError || t == anthropic.ErrorTypeAPIError) {
			return string(t)
		}
	case errors.As(err, &cut):
		switch {
		case errors.Is(err, errStalled):
			return "a stall"
		case errors.Is(err, errTruncated):
			return "a stream cut short"
		}
		return "a lost connection"
	}
	return ""
}

// wait pauses for d, or until the turn ends.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
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
