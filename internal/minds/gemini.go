package minds

import (
	"context"
	"encoding/json"
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

// Speak calls generateContent with the seat's thinking level. The seat's own
// past turns go back as returned, thought signatures included, when they came
// from the same model.
func (m *geminiMind) Speak(ctx context.Context, system string, turns []Turn) (Reply, error) {
	contents := make([]*genai.Content, 0, len(turns))
	for _, t := range turns {
		if t.Role == "user" {
			contents = append(contents, genai.NewContentFromText(t.Text, genai.RoleUser))
			continue
		}
		var c genai.Content
		if len(t.Raw) > 0 && t.Model == m.spec.Model && json.Unmarshal(t.Raw, &c) == nil {
			contents = append(contents, &c)
			continue
		}
		contents = append(contents, genai.NewContentFromText(t.Text, genai.RoleModel))
	}
	resp, err := m.client.Models.GenerateContent(ctx, m.spec.Model, contents, &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		MaxOutputTokens:   int32(m.opt.MaxTokens),
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(m.spec.Effort))},
	})
	if err != nil {
		return Reply{}, err
	}
	reply := Reply{Text: resp.Text(), Served: resp.ModelVersion}
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		reply.Raw, _ = json.Marshal(resp.Candidates[0].Content)
		reply.Stop = string(resp.Candidates[0].FinishReason)
	}
	if u := resp.UsageMetadata; u != nil {
		reply.Usage = Usage{
			Input:     int64(u.PromptTokenCount - u.CachedContentTokenCount),
			CacheRead: int64(u.CachedContentTokenCount),
			Output:    int64(u.CandidatesTokenCount + u.ThoughtsTokenCount),
			Thinking:  int64(u.ThoughtsTokenCount),
		}
	}
	return reply, nil
}
