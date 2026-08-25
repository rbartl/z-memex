package extraction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
}

func NewOllama(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{},
	}
}

type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	Format string `json:"format"`
	Think  bool   `json:"think"`
}

type ollamaGenerateResponse struct {
	Response        string `json:"response"`
	TotalDuration   int64  `json:"total_duration"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

func (o *Ollama) generate(ctx context.Context, prompt string) (*ollamaGenerateResponse, error) {
	body, _ := json.Marshal(ollamaGenerateRequest{
		Model:  o.model,
		Prompt: prompt,
		Stream: false,
		Format: "json",
		Think:  false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama generate request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama generate status: %d", resp.StatusCode)
	}

	var result ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama generate decode: %w", err)
	}

	return &result, nil
}

func (o *Ollama) Extract(ctx context.Context, content string) (*memory.ExtractionResult, error) {
	slog.Info("ollama extract starting", "model", o.model, "content_len", len(content))

	resp, err := o.generate(ctx, buildExtractionPrompt()+content)
	if err != nil {
		return nil, fmt.Errorf("ollama extraction: %w", err)
	}

	duration := time.Duration(resp.TotalDuration)
	slog.Info("ollama extract completed",
		"model", o.model,
		"duration", duration.Round(time.Millisecond),
		"prompt_tokens", resp.PromptEvalCount,
		"output_tokens", resp.EvalCount,
		"response_len", len(resp.Response),
	)

	if resp.Response == "" {
		slog.Warn("ollama extract returned empty response", "model", o.model)
		return nil, nil
	}

	var result memory.ExtractionResult
	if err := json.Unmarshal([]byte(resp.Response), &result); err != nil {
		return nil, fmt.Errorf("parse extraction result: %w (raw: %s)", err, resp.Response)
	}

	slog.Info("ollama extract parsed",
		"summary_len", len(result.Summary),
		"relations", len(result.Relations),
	)

	return &result, nil
}

const ollamaMergePrompt = `You are a knowledge base assistant. Merge these two pieces of knowledge into one concise statement. Preserve ALL unique facts. Remove redundancy. Return ONLY the merged text as a JSON object: {"merged": "..."}

Existing memory:
%s

New information:
%s`

func (o *Ollama) MergeMemories(ctx context.Context, existing, incoming string) (string, error) {
	slog.Info("ollama merge starting", "model", o.model)
	prompt := fmt.Sprintf(ollamaMergePrompt, existing, incoming)
	resp, err := o.generate(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("ollama merge: %w", err)
	}
	if resp.Response == "" {
		return incoming, nil
	}
	var result struct {
		Merged string `json:"merged"`
	}
	if err := json.Unmarshal([]byte(resp.Response), &result); err != nil {
		// If JSON parsing fails, return the raw response trimmed.
		return resp.Response, nil
	}
	if result.Merged == "" {
		return incoming, nil
	}
	return result.Merged, nil
}

func (o *Ollama) ExtractCrossRelations(ctx context.Context, contentA, contentB string) ([]memory.ExtractedRelation, error) {
	slog.Info("ollama cross-relation starting", "model", o.model, "content_a_len", len(contentA), "content_b_len", len(contentB))

	prompt := buildCrossRelationPrompt(contentA, contentB)

	resp, err := o.generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("ollama cross-relation extraction: %w", err)
	}

	duration := time.Duration(resp.TotalDuration)
	slog.Info("ollama cross-relation completed",
		"model", o.model,
		"duration", duration.Round(time.Millisecond),
		"prompt_tokens", resp.PromptEvalCount,
		"output_tokens", resp.EvalCount,
		"response_len", len(resp.Response),
	)

	if resp.Response == "" {
		slog.Warn("ollama cross-relation returned empty response", "model", o.model)
		return nil, nil
	}

	raw := resp.Response
	var relations []memory.ExtractedRelation
	if err := json.Unmarshal([]byte(raw), &relations); err != nil {
		// Model may return a single object instead of an array — wrap and retry.
		var single memory.ExtractedRelation
		if err2 := json.Unmarshal([]byte(raw), &single); err2 != nil {
			return nil, fmt.Errorf("parse cross-relations: %w (raw: %s)", err, raw)
		}
		relations = []memory.ExtractedRelation{single}
	}

	slog.Info("ollama cross-relation parsed", "relations", len(relations))

	return relations, nil
}
