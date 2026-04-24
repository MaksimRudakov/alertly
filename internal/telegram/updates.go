package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type Message struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

type editMessageTextRequest struct {
	ChatID                int64                 `json:"chat_id"`
	MessageID             int64                 `json:"message_id"`
	Text                  string                `json:"text"`
	ParseMode             string                `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool                  `json:"disable_web_page_preview"`
	ReplyMarkup           *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type editMessageReplyMarkupRequest struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

func (c *client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	req := getUpdatesRequest{
		Offset:         offset,
		Timeout:        int(timeout.Seconds()),
		AllowedUpdates: []string{"callback_query"},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal getUpdates: %w", err)
	}

	endpoint := c.endpoint("getUpdates")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Use a dedicated HTTP client so long-poll timeout does not clash with RequestTimeout.
	pollClient := &http.Client{Timeout: timeout + c.cfg.RequestTimeout}
	resp, err := pollClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("getUpdates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
		var ar apiResponse
		if err := json.Unmarshal(respBody, &ar); err == nil {
			if ar.Description != "" {
				ae.Body = ar.Description
			}
			if ar.Parameters != nil && ar.Parameters.RetryAfter > 0 {
				ae.RetryAfter = time.Duration(ar.Parameters.RetryAfter) * time.Second
			}
		}
		return nil, ae
	}

	var ar struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !ar.OK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: "ok=false"}
	}
	return ar.Result, nil
}

func (c *client) AnswerCallbackQuery(ctx context.Context, callbackID, text string, showAlert bool) error {
	if c.cfg.DryRun {
		c.log.Info("dry run: skip answerCallbackQuery", "id", callbackID, "text", text)
		return nil
	}
	req := answerCallbackQueryRequest{
		CallbackQueryID: callbackID,
		Text:            text,
		ShowAlert:       showAlert,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal answerCallbackQuery: %w", err)
	}
	_, err = c.callWithRetry(ctx, c.endpoint("answerCallbackQuery"), body)
	return err
}

func (c *client) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, markup *InlineKeyboardMarkup) error {
	if c.cfg.DryRun {
		c.log.Info("dry run: skip editMessageText", "chat_id", chatID, "message_id", messageID)
		return nil
	}
	req := editMessageTextRequest{
		ChatID:                chatID,
		MessageID:             messageID,
		Text:                  text,
		ParseMode:             c.cfg.ParseMode,
		DisableWebPagePreview: true,
		ReplyMarkup:           markup,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal editMessageText: %w", err)
	}
	_, err = c.callWithRetry(ctx, c.endpoint("editMessageText"), body)
	return err
}

func (c *client) EditMessageReplyMarkup(ctx context.Context, chatID int64, messageID int64, markup *InlineKeyboardMarkup) error {
	if c.cfg.DryRun {
		c.log.Info("dry run: skip editMessageReplyMarkup", "chat_id", chatID, "message_id", messageID)
		return nil
	}
	req := editMessageReplyMarkupRequest{
		ChatID:      chatID,
		MessageID:   messageID,
		ReplyMarkup: markup,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal editMessageReplyMarkup: %w", err)
	}
	_, err = c.callWithRetry(ctx, c.endpoint("editMessageReplyMarkup"), body)
	return err
}
