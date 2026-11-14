package minds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/genai"
)

type geminiMind struct {
	client *genai.Client
	spec   Spec
	opt    Options
}

func newGemini(spec Spec, key string, opt Options) (Mind, error) {
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return nil, err
	}
	return &geminiMind{client: client, spec: spec, opt: opt}, nil
}

// config builds the request settings: the seat's thinking level, Google
// Search, and the local tools as functions. Mixing a built-in tool with
// functions needs includeServerSideToolInvocations (a 400 without it); the
// search steps then come back in the content, signed, for replay. last
// stops function calls once the turn has used its share.
func (m *geminiMind) config(system string, last bool) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		MaxOutputTokens:   int32(m.opt.MaxTokens),
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(m.spec.Effort))},
		ToolConfig:        &genai.ToolConfig{},
	}
	if m.spec.NativeSearch() {
		cfg.Tools = append(cfg.Tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	if tools := m.opt.Tools.Tools(); len(tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, len(tools))
		for i, t := range tools {
			decls[i] = &genai.FunctionDeclaration{Name: t.Name, Description: t.Description, ParametersJsonSchema: t.Schema()}
		}
		cfg.Tools = append(cfg.Tools, &genai.Tool{FunctionDeclarations: decls})
		if m.spec.NativeSearch() {
			cfg.ToolConfig.IncludeServerSideToolInvocations = genai.Ptr(true)
		}
		if last {
			cfg.ToolConfig.FunctionCallingConfig = &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}
		}
	}
	return cfg
}

// Speak calls generateContent, looping on function calls until the mind
// answers. The seat's own past turns go back as returned, thought signatures
// included, when they came from the same model.
func (m *geminiMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	var history []*genai.Content
	for _, t := range turns {
		if t.Role == "user" {
			history = append(history, genai.NewContentFromText(t.Text, genai.RoleUser))
		} else {
			history = append(history, geminiReplay(t, m.spec.Model)...)
		}
	}
	var reply Reply
	var exchange []*genai.Content
	calls, nudged := 0, false
	for step := 0; step < maxSteps; step++ {
		contents := append(append([]*genai.Content(nil), history...), exchange...)
		resp, err := m.client.Models.GenerateContent(ctx, m.spec.Model, contents, m.config(system, calls >= m.opt.maxCalls()))
		if err != nil {
			return reply, err
		}
		reply.Served = resp.ModelVersion
		if u := resp.UsageMetadata; u != nil {
			reply.Usage.add(Usage{
				Input:     int64(u.PromptTokenCount - u.CachedContentTokenCount),
				CacheRead: int64(u.CachedContentTokenCount),
				Output:    int64(u.CandidatesTokenCount + u.ThoughtsTokenCount),
				Thinking:  int64(u.ThoughtsTokenCount),
			})
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			if len(resp.Candidates) > 0 {
				reply.Stop = string(resp.Candidates[0].FinishReason)
			} else if resp.PromptFeedback != nil {
				reply.Stop = string(resp.PromptFeedback.BlockReason)
			}
			reply.Raw, err = json.Marshal(exchange)
			return reply, err
		}
		cand := resp.Candidates[0]
		if g := cand.GroundingMetadata; g != nil {
			for _, q := range g.WebSearchQueries {
				reply.Usage.Searches++
				searched(m.opt, &reply, q)
			}
		}
		exchange = append(exchange, cand.Content)
		var results []*genai.Part
		for _, p := range cand.Content.Parts {
			if p.FunctionCall == nil {
				continue
			}
			calls++
			input, _ := json.Marshal(p.FunctionCall.Args)
			out, failed := call(ctx, m.opt, &reply, p.FunctionCall.Name, input)
			key := "output"
			if failed {
				key = "error"
			}
			results = append(results, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Response: map[string]any{key: spent(out, calls, m.opt)},
			}})
		}
		if len(results) == 0 {
			if strings.TrimSpace(resp.Text()) == "" && cand.FinishReason == genai.FinishReasonStop && !nudged {
				nudged = true
				exchange = append(exchange, genai.NewContentFromText(nudge, genai.RoleUser))
				continue
			}
			reply.Text, reply.Stop = resp.Text(), string(cand.FinishReason)
			reply.Raw, err = json.Marshal(exchange)
			return reply, err
		}
		exchange = append(exchange, &genai.Content{Role: genai.RoleUser, Parts: results})
	}
	return reply, errors.New("the turn ran past its request limit")
}

// geminiReplay sends a past turn back as it ran, when it came from the same
// model: every content of its exchange, signatures included. A turn logged
// before tools existed is one content; anything else goes back as text.
func geminiReplay(t Turn, model string) []*genai.Content {
	if len(t.Raw) > 0 && t.Model == model {
		var exchange []*genai.Content
		if json.Unmarshal(t.Raw, &exchange) == nil && len(exchange) > 0 {
			return exchange
		}
		var one genai.Content
		if json.Unmarshal(t.Raw, &one) == nil && len(one.Parts) > 0 {
			return []*genai.Content{&one}
		}
	}
	return []*genai.Content{genai.NewContentFromText(t.Text, genai.RoleModel)}
}
