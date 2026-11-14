package minds

import (
	"context"
	"encoding/json"

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
}

func newOpenAI(spec Spec, key string, opt Options) Mind {
	return &openaiMind{client: openai.NewClient(option.WithAPIKey(key)), spec: spec, opt: opt}
}

// Speak calls the Responses API statelessly (store is off: the log is the
// state). The seat's own past turns go back as the output items it returned,
// encrypted reasoning included, when they came from the same model.
func (m *openaiMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	var input responses.ResponseInputParam
	for _, t := range turns {
		var items []json.RawMessage
		if t.Role == "assistant" && len(t.Raw) > 0 && t.Model == m.spec.Model && json.Unmarshal(t.Raw, &items) == nil {
			for _, item := range items {
				input = append(input, param.Override[responses.ResponseInputItemUnionParam](item))
			}
			continue
		}
		role := responses.EasyInputMessageRoleUser
		if t.Role == "assistant" {
			role = responses.EasyInputMessageRoleAssistant
		}
		input = append(input, responses.ResponseInputItemParamOfMessage(t.Text, role))
	}
	resp, err := m.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:           shared.ResponsesModel(m.spec.Model),
		Instructions:    openai.String(system),
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Reasoning:       shared.ReasoningParam{Effort: shared.ReasoningEffort(m.spec.Effort)},
		Store:           openai.Bool(false),
		Include:         []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		MaxOutputTokens: openai.Int(m.opt.MaxTokens),
		PromptCacheKey:  openai.String(m.opt.Session + "/" + m.spec.Seat),
	})
	if err != nil {
		return Reply{}, err
	}
	var raw struct {
		Output json.RawMessage `json:"output"`
	}
	_ = json.Unmarshal([]byte(resp.RawJSON()), &raw)
	u := resp.Usage
	reply := Reply{
		Text:   resp.OutputText(),
		Raw:    raw.Output,
		Served: string(resp.Model),
		Stop:   string(resp.Status),
		Usage: Usage{
			Input:      max(0, u.InputTokens-u.InputTokensDetails.CachedTokens-u.InputTokensDetails.CacheWriteTokens),
			CacheRead:  u.InputTokensDetails.CachedTokens,
			CacheWrite: u.InputTokensDetails.CacheWriteTokens,
			Output:     u.OutputTokens,
			Thinking:   u.OutputTokensDetails.ReasoningTokens,
		},
	}
	if resp.Status == responses.ResponseStatusIncomplete {
		reply.Stop = "incomplete " + resp.IncompleteDetails.Reason
	}
	return reply, nil
}
