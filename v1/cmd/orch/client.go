package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// client talks to orchd.
//
// Deliberately thin: every decision -- who may deploy, whether a rollback is
// safe, which version a promotion ships -- is made by the server. Section 3
// chose a central server precisely so that a second copy of those rules does
// not live in every engineer's CLI, quietly drifting from the first.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// No overall timeout: `--wait` polls for as long as a bake takes, and
		// each request gets its own context deadline instead.
		http: &http.Client{},
	}
}

// apiError carries the server's message and status.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

func (c *client) do(ctx context.Context, method, path string, body any, out any, headers map[string]string) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The most common cause by a wide margin is that orchd is not running,
		// and "connection refused" on its own sends people to look at the
		// network. Say what it means.
		return fmt.Errorf("cannot reach orchd at %s: %w\n"+
			"  Is it running? Set ORCH_SERVER or pass --server if it is somewhere else.", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding the server's reply: %w", err)
		}
	}
	return nil
}

func (c *client) text(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach orchd at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &apiError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return string(b), nil
}

func requestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func serverURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("ORCH_SERVER"); v != "" {
		return v
	}
	return "http://127.0.0.1:8080"
}

func apiToken(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	// A file before an environment variable: an env var is visible in `ps` on
	// some systems and leaks into child processes and crash reports.
	if path := os.Getenv("ORCH_TOKEN_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv("ORCH_TOKEN")
}

var errNoToken = errors.New(
	"no API token: set ORCH_TOKEN, or ORCH_TOKEN_FILE, or pass --token\n" +
		"  Mint one with: orchd token issue --user you")
