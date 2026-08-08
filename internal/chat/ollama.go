package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Message is a single turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// StreamCallbacks holds the callbacks invoked during a streaming response.
// OnThinking may be nil — in that case thinking tokens are silently ignored.
type StreamCallbacks struct {
	OnToken    func(string) error
	OnThinking func(string) error
}

// Client streams chat completions from the Ollama /api/chat endpoint.
type Client struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewClient returns a new chat Client.
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{},
	}
}

// Model returns the configured model name.
func (c *Client) Model() string {
	return c.model
}

type ollamaChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Think    bool      `json:"think"`
}

// ollamaResponseMessage is the message field in a streaming chunk.
// It extends the request Message with an optional Thinking field returned
// by models that support extended reasoning (e.g. qwq, deepseek-r1).
type ollamaResponseMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking"`
}

type ollamaChatChunk struct {
	Message ollamaResponseMessage `json:"message"`
	Done    bool                  `json:"done"`
}

// Stream calls Ollama /api/chat with streaming and think:true enabled.
// Thinking tokens (if the model produces them) are forwarded to cbs.OnThinking;
// response tokens are forwarded to cbs.OnToken. Either callback may return an
// error to abort the stream early.
func (c *Client) Stream(ctx context.Context, messages []Message, cbs StreamCallbacks) error {
	body, err := json.Marshal(ollamaChatRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   true,
		Think:    true, // no-op for models that don't support it
	})
	if err != nil {
		return fmt.Errorf("chat: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("chat: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("chat: do request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chat: ollama status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			return fmt.Errorf("chat: decode chunk: %w", err)
		}

		if chunk.Message.Thinking != "" && cbs.OnThinking != nil {
			if err := cbs.OnThinking(chunk.Message.Thinking); err != nil {
				return err
			}
		}

		if chunk.Message.Content != "" {
			if err := cbs.OnToken(chunk.Message.Content); err != nil {
				return err
			}
		}

		if chunk.Done {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("chat: read stream: %w", err)
	}

	return nil
}

const queryRewritePrompt = `You are a search query synthesizer for a personal knowledge base.

Given the conversation below, output the single most useful search query to retrieve relevant memories that would help answer the user's latest message. Take into account the full conversation context to resolve pronouns and references (e.g. "her" → the person mentioned earlier).

The knowledge base stores facts about the user: their preferences, projects, tools, decisions, colleagues, and belongings. Assume the answer may well be in there — questions about what the user owns, uses, prefers, or has decided are exactly what it is for.

Reply with ONLY the raw search query string — no quotes, no explanation, no punctuation at the end.`

// maxSynthesizedQueryLen bounds a synthesized query. Anything longer means the
// model ignored the instruction and wrote prose, which makes a worse search
// query than the user's own words.
const maxSynthesizedQueryLen = 300

// cleanQuery normalizes a synthesized query, falling back to the user's raw
// message whenever the model's answer is unusable.
//
// The fallback matters more than the rewrite: an empty query used to skip
// retrieval altogether, so one uncooperative response silently cost the model
// its entire knowledge base. Searching with the user's own words is always
// better than searching with nothing.
func cleanQuery(raw, fallback string) string {
	// Models sometimes add a preamble or trailing commentary; the query itself
	// is on the first non-empty line.
	query := ""
	for line := range strings.SplitSeq(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			query = line
			break
		}
	}

	query = strings.TrimSpace(strings.Trim(strings.TrimSpace(query), `"'`))

	// "none" is not an instruction any more, but models still volunteer it for
	// greetings — treat it as "no useful rewrite", not "do not search".
	if query == "" || strings.EqualFold(query, "none") || len(query) > maxSynthesizedQueryLen {
		return fallback
	}
	return query
}

// SynthesizeQuery sends the conversation to the model and asks it to produce
// the best knowledge-base search query given the full context. It never
// returns an empty string: if the model's answer is unusable, the user's own
// message is returned so retrieval still happens.
func (c *Client) SynthesizeQuery(ctx context.Context, history []Message, latestUserMsg string) (string, error) {
	// Build a compact conversation transcript for the prompt.
	var sb strings.Builder
	for _, m := range history {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	fmt.Fprintf(&sb, "user: %s", latestUserMsg)

	messages := []Message{
		{Role: "system", Content: queryRewritePrompt},
		{Role: "user", Content: sb.String()},
	}

	body, err := json.Marshal(ollamaChatRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   false,
		Think:    false, // keep query synthesis fast — no reasoning needed
	})
	if err != nil {
		return "", fmt.Errorf("chat: synthesize query marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("chat: synthesize query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat: synthesize query do: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat: synthesize query status %d", resp.StatusCode)
	}

	var result ollamaChatChunk
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("chat: synthesize query decode: %w", err)
	}

	return cleanQuery(result.Message.Content, latestUserMsg), nil
}
