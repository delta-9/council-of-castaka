package minds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type openaiMind struct {
	client openai.Client
	spec   Spec
	opt    Options
	tools  []responses.ToolUnionParam
}

func newOpenAI(spec Spec, key string, opt Options) Mind {
	m := &openaiMind{client: openai.NewClient(option.WithAPIKey(key)), spec: spec, opt: opt}
	if spec.NativeSearch() {
		m.tools = append(m.tools, responses.ToolUnionParam{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}})
	}
	for _, t := range opt.Tools.Tools() {
		m.tools = append(m.tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: t.Name, Description: openai.String(t.Description), Parameters: t.Schema(), Strict: openai.Bool(false),
		}})
	}
	return m
}

// openaiItem is what the loop reads of an output item.
type openaiItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Action    struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	} `json:"action"`
}

// Speak calls the Responses API statelessly (store is off: the log is the
// state), looping on function calls until the mind answers. The seat's own
// past turns go back as the items they produced, encrypted reasoning
// included, when they came from the same model.
func (m *openaiMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	var history responses.ResponseInputParam
	for _, t := range turns {
		if t.Role == "assistant" {
			if items := openaiReplay(t, m.spec.Model); items != nil {
				history = append(history, items...)
				continue
			}
		}
		role := responses.EasyInputMessageRoleUser
		if t.Role == "assistant" {
			role = responses.EasyInputMessageRoleAssistant
		}
		history = append(history, responses.ResponseInputItemParamOfMessage(t.Text, role))
	}
	var reply Reply
	var exchange []json.RawMessage
	calls, nudged := 0, false
	for step := 0; step < maxSteps; step++ {
		input := append(responses.ResponseInputParam(nil), history...)
		for _, item := range exchange {
			input = append(input, param.Override[responses.ResponseInputItemUnionParam](item))
		}
		params := responses.ResponseNewParams{
			Model:           shared.ResponsesModel(m.spec.Model),
			Instructions:    openai.String(system),
			Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
			Reasoning:       shared.ReasoningParam{Effort: shared.ReasoningEffort(m.spec.Effort)},
			Store:           openai.Bool(false),
			Include:         []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
			MaxOutputTokens: openai.Int(m.opt.MaxTokens),
			PromptCacheKey:  openai.String(m.opt.Session + "/" + m.spec.Seat),
			Tools:           m.tools,
		}
		if calls >= m.opt.maxCalls() {
			params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsNone)}
		}
		resp, err := m.client.Responses.New(ctx, params)
		if err != nil {
			return reply, err
		}
		u := resp.Usage
		reply.Served = string(resp.Model)
		reply.Usage.add(Usage{
			Input:      max(0, u.InputTokens-u.InputTokensDetails.CachedTokens-u.InputTokensDetails.CacheWriteTokens),
			CacheRead:  u.InputTokensDetails.CachedTokens,
			CacheWrite: u.InputTokensDetails.CacheWriteTokens,
			Output:     u.OutputTokens,
			Thinking:   u.OutputTokensDetails.ReasoningTokens,
		})
		var out struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal([]byte(resp.RawJSON()), &out); err != nil {
			return reply, err
		}
		exchange = append(exchange, out.Output...)
		var results []json.RawMessage
		for _, raw := range out.Output {
			var item openaiItem
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch item.Type {
			case "web_search_call":
				if item.Action.Type == "search" || item.Action.Type == "" {
					reply.Usage.Searches++
				}
				for _, q := range append([]string{item.Action.Query}, item.Action.Queries...) {
					if q != "" {
						searched(m.opt, &reply, q)
					}
				}
			case "function_call":
				calls++
				result, _ := call(ctx, m.opt, &reply, item.Name, json.RawMessage(item.Arguments))
				b, err := json.Marshal(map[string]string{"type": "function_call_output", "call_id": item.CallID, "output": spent(result, calls, m.opt)})
				if err != nil {
					return reply, err
				}
				results = append(results, b)
			}
		}
		if len(results) == 0 {
			if strings.TrimSpace(resp.OutputText()) == "" && resp.Status == responses.ResponseStatusCompleted && !nudged {
				nudged = true
				b, err := json.Marshal(map[string]string{"role": "user", "content": nudge})
				if err != nil {
					return reply, err
				}
				exchange = append(exchange, b)
				continue
			}
			reply.Text, reply.Stop = resp.OutputText(), string(resp.Status)
			if resp.Status == responses.ResponseStatusIncomplete {
				reply.Stop = "incomplete " + resp.IncompleteDetails.Reason
			}
			reply.Raw, err = json.Marshal(exchange)
			return reply, err
		}
		exchange = append(exchange, results...)
	}
	return reply, errors.New("the turn ran past its request limit")
}

// openaiReplay returns a past turn's items as input, when they came from the
// same model; nil means the turn goes back as text.
func openaiReplay(t Turn, model string) responses.ResponseInputParam {
	var items []json.RawMessage
	if len(t.Raw) == 0 || t.Model != model || json.Unmarshal(t.Raw, &items) != nil {
		return nil
	}
	input := make(responses.ResponseInputParam, len(items))
	for i, item := range items {
		input[i] = param.Override[responses.ResponseInputItemUnionParam](item)
	}
	return input
}
