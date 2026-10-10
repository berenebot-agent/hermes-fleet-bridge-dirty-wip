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
	"unicode/utf16"
)

type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID       int    `json:"message_id"`
		Text            string `json:"text"`
		MessageThreadID int    `json:"message_thread_id"`
		Chat            struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
	} `json:"message"`
}
type Telegram struct {
	client      *http.Client
	token, base string
	group       int64
}

func newTelegram(token string, group int64) *Telegram {
	return &Telegram{client: &http.Client{Timeout: 40 * time.Second}, token: token, base: "https://api.telegram.org/bot" + token + "/", group: group}
}
func (t *Telegram) call(ctx context.Context, method string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.base+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram %s request: %s", method, strings.ReplaceAll(err.Error(), t.token, "[REDACTED]"))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("Telegram %s HTTP %d invalid JSON", method, res.StatusCode)
	}
	if !envelope.OK {
		return fmt.Errorf("Telegram %s HTTP %d: %.240s", method, res.StatusCode, envelope.Description)
	}
	if out != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func (t *Telegram) deleteWebhook(ctx context.Context) error {
	return t.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}
func (t *Telegram) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var list []Update
	err := t.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "limit": 100, "allowed_updates": []string{"message"}}, &list)
	return list, err
}
func (t *Telegram) createTopic(ctx context.Context, title string) (Topic, error) {
	var v struct {
		MessageThreadID int    `json:"message_thread_id"`
		Name            string `json:"name"`
	}
	err := t.call(ctx, "createForumTopic", map[string]any{"chat_id": t.group, "name": title}, &v)
	if err != nil {
		return Topic{}, err
	}
	if v.MessageThreadID == 0 {
		return Topic{}, fmt.Errorf("Telegram created topic with missing ID")
	}
	return Topic{ID: v.MessageThreadID, Title: title}, nil
}
func (t *Telegram) editTopic(ctx context.Context, id int, title string) error {
	err := t.call(ctx, "editForumTopic", map[string]any{"chat_id": t.group, "message_thread_id": id, "name": title}, nil)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "topic_not_modified") {
		return nil
	}
	return err
}
func (t *Telegram) typing(ctx context.Context, thread int) {
	body := t.threadBody(thread)
	body["action"] = "typing"
	_ = t.call(ctx, "sendChatAction", body, nil)
}
func (t *Telegram) sendMessage(ctx context.Context, thread int, text string) error {
	body := t.threadBody(thread)
	body["text"] = text
	return t.call(ctx, "sendMessage", body, nil)
}

// General (the forum's built-in topic) is addressed by omitting message_thread_id.
func (t *Telegram) threadBody(thread int) map[string]any {
	body := map[string]any{"chat_id": t.group}
	if thread > generalThread {
		body["message_thread_id"] = thread
	}
	return body
}
func splitTelegram(s string) []string {
	var out []string
	var b strings.Builder
	units := 0
	for _, r := range s {
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if units+n > 3500 && units > 0 {
			out = append(out, b.String())
			b.Reset()
			units = 0
		}
		b.WriteRune(r)
		units += n
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	if len(out) == 0 {
		return []string{"(Hermes returned an empty response.)"}
	}
	return out
}
