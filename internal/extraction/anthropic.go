package extraction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

type Anthropic struct {
	client *anthropic.Client
}

func NewAnthropic(apiKey string) *Anthropic {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Anthropic{client: &client}
}

func buildExtractionPrompt() string {
	typeList := strings.Join(memory.PromptEntityTypes, ", ")
	relList := strings.Join(memory.Relationships, ", ")
	return `You are a knowledge graph extractor and summarizer. Given a piece of text, do two things:

1. Write a one-sentence summary that captures the key fact or decision.
2. Extract named entities and the relationships between them.

Return ONLY a JSON object with this shape:
{
  "summary": "one clear sentence summarizing the key point",
  "relations": [
    {
      "from_entity": "entity name",
      "from_type": "entity type (see rules)",
      "to_entity": "entity name",
      "to_type": "entity type (see rules)",
      "relationship": "one predicate from the allowed list (see rules)",
      "confidence": 0.0 to 1.0
    }
  ]
}

Rules:
- The summary should be self-contained and useful without the original text
- Only extract meaningful, non-trivial relations
- Extract an entity ONLY if it is a specific named thing: a person, an organisation, a team, a project, a product, a named technology or tool, a document, an event, or a place. A useful test: would this same entity plausibly appear in a different, unrelated note? "PostgreSQL" and "Acme Corp" pass. "reduced overhead", "the attached spreadsheet", "improved workflow" do not.
- Do NOT extract abstract noun phrases, descriptions, quantities, states, or wording lifted from this text just because it appears. Half the entities in a knowledge base connect nothing if this rule is ignored.
- Prefer the shortest name that still identifies the thing ("Gemini Vision", not "the Gemini Vision capability")
- Entity types MUST be one of: ` + typeList + `
- If no listed type fits, do NOT extract the entity. Never invent a type.
- Relationships MUST be one of: ` + relList + `
- Each predicate has a fixed direction: write from_entity as the subject. Prefer the most specific predicate that is true; use "relates-to" only when nothing else fits.
- Do not invent a predicate, and do not state one backwards ("Acme employs Max" should be from_entity "Max", relationship "works-at", to_entity "Acme").
- Use an empty array for relations if no meaningful relations found
- Fewer, better entities beat more. An empty array is a valid answer.
- No explanation, no markdown, only the JSON object

Text to analyze:
`
}

func buildCrossRelationPrompt(contentA, contentB string) string {
	typeList := strings.Join(memory.PromptEntityTypes, ", ")
	relList := strings.Join(memory.Relationships, ", ")
	return `You are a knowledge graph extractor. You are given two separate pieces of knowledge from a personal knowledge base. Identify relationships that connect entities ACROSS these two texts.

Return ONLY a JSON array of relations. Each relation has this shape:
{
  "from_entity": "entity name (from either text)",
  "from_type": "entity type (see rules)",
  "to_entity": "entity name (from either text)",
  "to_type": "entity type (see rules)",
  "relationship": "one predicate from the allowed list (see rules)",
  "confidence": 0.0 to 1.0
}

Rules:
- Only extract relationships that CONNECT the two texts (not relations within a single text)
- Extract an entity ONLY if it is a specific named thing: a person, an organisation, a team, a project, a product, a named technology or tool, a document, an event, or a place. A useful test: would this same entity plausibly appear in a different, unrelated note? "PostgreSQL" and "Acme Corp" pass. "reduced overhead", "the attached spreadsheet", "improved workflow" do not.
- Do NOT extract abstract noun phrases, descriptions, quantities, states, or wording lifted from this text just because it appears. Half the entities in a knowledge base connect nothing if this rule is ignored.
- Prefer the shortest name that still identifies the thing ("Gemini Vision", not "the Gemini Vision capability")
- Entity types MUST be one of: ` + typeList + `
- If no listed type fits, do NOT extract the entity. Never invent a type.
- Relationships MUST be one of: ` + relList + `
- Each predicate has a fixed direction: write from_entity as the subject. Prefer the most specific predicate that is true; use "relates-to" only when nothing else fits.
- Do not invent a predicate, and do not state one backwards ("Acme employs Max" should be from_entity "Max", relationship "works-at", to_entity "Acme").
- Return [] if no meaningful cross-text relations found
- Fewer, better entities beat more. [] is a valid answer.
- No explanation, no markdown, only the JSON array

Text A:
` + contentA + `

Text B:
` + contentB + `
`
}

func (a *Anthropic) Extract(ctx context.Context, content string) (*memory.ExtractionResult, error) {
	prompt := buildExtractionPrompt() + content
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5, // cheap + fast
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic extraction: %w", err)
	}

	if len(msg.Content) == 0 {
		return nil, nil
	}

	raw := msg.Content[0].Text
	raw = strings.TrimSpace(raw)

	// strip markdown fences if present
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var result memory.ExtractionResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parse extraction result: %w (raw: %s)", err, raw)
	}

	return &result, nil
}

const mergePrompt = `You are a knowledge base assistant. You have an existing memory and new incoming information about the same topic. Merge them into a single, concise statement that:
- Preserves ALL unique facts from both texts
- Removes redundancy and duplication
- Keeps the tone factual and self-contained
- Does NOT add any information not present in either text

Return ONLY the merged text, no explanation or formatting.

Existing memory:
%s

New information:
%s

Merged result:`

func (a *Anthropic) MergeMemories(ctx context.Context, existing, incoming string) (string, error) {
	prompt := fmt.Sprintf(mergePrompt, existing, incoming)
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("anthropic merge: %w", err)
	}
	if len(msg.Content) == 0 {
		return incoming, nil
	}
	return strings.TrimSpace(msg.Content[0].Text), nil
}

func (a *Anthropic) ExtractCrossRelations(ctx context.Context, contentA, contentB string) ([]memory.ExtractedRelation, error) {
	prompt := buildCrossRelationPrompt(contentA, contentB)

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic cross-relation extraction: %w", err)
	}

	if len(msg.Content) == 0 {
		return nil, nil
	}

	raw := msg.Content[0].Text
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var relations []memory.ExtractedRelation
	if err := json.Unmarshal([]byte(raw), &relations); err != nil {
		return nil, fmt.Errorf("parse cross-relations: %w (raw: %s)", err, raw)
	}

	return relations, nil
}
