package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Hermes struct {
	client    *http.Client
	base, key string
}

func newHermes(base, key string) *Hermes {
	return &Hermes{client: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, base: base, key: key}
}
func (h *Hermes) request(ctx context.Context, method, path string, body any, headers map[string]string, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 2097152))
	if err != nil {
		return res.StatusCode, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Hermes may embed user-controlled data in JSON errors. Do not leak API key in logs.
		msg := strings.ReplaceAll(string(b), h.key, "[REDACTED]")
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return res.StatusCode, fmt.Errorf("Hermes HTTP %d: %s", res.StatusCode, msg)
	}
	if out != nil && len(b) > 0 {
		if err = json.Unmarshal(b, out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}
func (h *Hermes) ready(ctx context.Context) error {
	_, err := h.request(ctx, "GET", "/health", nil, nil, nil)
	return err
}
func (h *Hermes) startRun(ctx context.Context, session, sessionKey, input, idempotency string) (string, error) {
	var v struct {
		RunID string `json:"run_id"`
	}
	_, err := h.request(ctx, "POST", "/v1/runs", map[string]any{"input": input, "session_id": session}, map[string]string{
		"Idempotency-Key":      idempotency,
		"X-Hermes-Session-Id":  session,
		"X-Hermes-Session-Key": sessionKey,
	}, &v)
	if err != nil {
		return "", err
	}
	if v.RunID == "" {
		return "", fmt.Errorf("Hermes didn't return run_id")
	}
	return v.RunID, nil
}

type RunStatus struct {
	Status string `json:"status"`
	Output string `json:"output"`
	Error  any    `json:"error"`
}

func (h *Hermes) status(ctx context.Context, runID string) (RunStatus, error) {
	var status RunStatus
	_, err := h.request(ctx, "GET", "/v1/runs/"+runID, nil, nil, &status)
	return status, err
}
func (h *Hermes) stopRun(ctx context.Context, runID string) error {
	_, err := h.request(ctx, "POST", "/v1/runs/"+runID+"/stop", map[string]any{}, nil, nil)
	return err
}
func (h *Hermes) wait(ctx context.Context, runID string) (RunStatus, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s, err := h.status(ctx, runID)
		if err != nil {
			return s, err
		}
		switch s.Status {
		case "completed", "failed", "cancelled", "interrupted":
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-ticker.C:
		}
	}
}
