// Package aibot is a tiny Ollama Cloud (OpenAI-compatible) chat client used to
// auto-reply to inbound WhatsApp messages. Self-contained so metaapi stays a
// single small binary; it reuses the same OLLAMA_API_KEY the rest of Greenpark
// uses. No streaming — one request, one reply.
package aibot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	key      string
	model    string
	endpoint string
	http     *http.Client
}

// New builds a client. Empty model/endpoint fall back to the Greenpark defaults
// (Ollama Cloud, glm-5.2:cloud). A blank key ⇒ Configured() is false.
func New(key, model, endpoint string) *Client {
	if strings.TrimSpace(model) == "" {
		model = "glm-5.2:cloud"
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "https://ollama.com/v1"
	}
	return &Client{
		key:      strings.TrimSpace(key),
		model:    strings.TrimSpace(model),
		endpoint: strings.TrimRight(endpoint, "/"),
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Configured() bool { return c != nil && c.key != "" }
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// Msg is one chat turn. Role is "system" | "user" | "assistant".
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Reply sends the system prompt + history to the model and returns the reply
// text. Context lets the caller bound the wait.
func (c *Client) Reply(ctx context.Context, system string, history []Msg) (string, error) {
	if !c.Configured() {
		return "", errors.New("aibot: API key belum diset")
	}
	msgs := make([]Msg, 0, len(history)+1)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, Msg{Role: "system", Content: system})
	}
	msgs = append(msgs, history...)

	payload, _ := json.Marshal(map[string]any{
		"model":       c.model,
		"messages":    msgs,
		"temperature": 0.4,
		"stream":      false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		snip := string(raw)
		if len(snip) > 240 {
			snip = snip[:240]
		}
		return "", fmt.Errorf("aibot: HTTP %d: %s", resp.StatusCode, snip)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("aibot: parse: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("aibot: respons kosong")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
