package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HTTPTransport is the real Slack Web API client.
//
// Written against net/http rather than slack-go for the reason section 14.2
// gives: a tool that holds production credentials pays for every dependency in
// supply-chain surface, and the three calls used here are three POSTs with a
// bearer token.
//
// Nothing in this file decides anything. Every decision -- who may approve,
// whether to update or repost, how to back off -- lives on the other side of
// the Transport interface and is tested against a fake.
type HTTPTransport struct {
	Token   string
	Client  *http.Client
	BaseURL string
}

func NewHTTPTransport(token string) *HTTPTransport {
	return &HTTPTransport{
		Token: token,
		// A timeout, always: Slack occasionally holds a connection open, and
		// an outbox worker blocked on a socket stops every notification, not
		// just this one.
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://slack.com/api",
	}
}

func (t *HTTPTransport) base() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return "https://slack.com/api"
}

func (t *HTTPTransport) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (t *HTTPTransport) PostMessage(ctx context.Context, m Message) (string, error) {
	var res struct {
		TS string `json:"ts"`
	}
	err := t.call(ctx, "chat.postMessage", map[string]any{
		"channel": m.Channel,
		"text":    m.Text,
		"blocks":  m.Blocks,
	}, &res)
	return res.TS, err
}

func (t *HTTPTransport) UpdateMessage(ctx context.Context, m Message) error {
	return t.call(ctx, "chat.update", map[string]any{
		"channel": m.Channel,
		"ts":      m.TS,
		"text":    m.Text,
		"blocks":  m.Blocks,
	}, nil)
}

func (t *HTTPTransport) PostEphemeral(ctx context.Context, channel, user, text string) error {
	return t.call(ctx, "chat.postEphemeral", map[string]any{
		"channel": channel,
		"user":    user,
		"text":    text,
	}, nil)
}

func (t *HTTPTransport) call(ctx context.Context, method string, body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.base()+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+t.Token)

	resp, err := t.client().Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer resp.Body.Close()

	// A 429 is carried as a typed error with Slack's own Retry-After, so the
	// outbox backs off by the amount asked for rather than guessing (section
	// 10.4). Guessing low here is what turns one throttle into a sustained one.
	if resp.StatusCode == http.StatusTooManyRequests {
		after := 30 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				after = time.Duration(n) * time.Second
			}
		}
		return &RateLimitedError{RetryAfter: after}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
	}

	// Slack answers 200 with `{"ok": false}` for application errors, so the
	// status code alone is not success.
	var envelope struct {
		OK    bool            `json:"ok"`
		Error string          `json:"error"`
		Raw   json.RawMessage `json:"-"`
	}
	dec := json.NewDecoder(resp.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("slack: %s: decoding response: %w", method, err)
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("slack: %s: decoding response: %w", method, err)
	}
	if !envelope.OK {
		if envelope.Error == "ratelimited" {
			return &RateLimitedError{RetryAfter: 30 * time.Second}
		}
		return fmt.Errorf("slack: %s: %s", method, envelope.Error)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("slack: %s: decoding response: %w", method, err)
		}
	}
	return nil
}
