package server

import (
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/notification"
)

func TestKeyboard_FiringAllowlistedChat(t *testing.T) {
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h", "4h"},
		ChatAllowlist: []int64{-100},
		Cache:         cache,
	}
	opts := k.Build(
		notification.ChatTarget{ChatID: -100},
		notification.Notification{
			Status:      "firing",
			Fingerprint: "fp",
			Labels:      map[string]string{"a": "1"},
			Links:       []notification.Link{{Title: "Runbook", URL: "https://rb"}},
		},
		"alertmanager",
	)
	if opts == nil || opts.ReplyMarkup == nil {
		t.Fatal("expected keyboard")
	}
	rows := opts.ReplyMarkup.InlineKeyboard
	if len(rows) != 1 {
		t.Fatalf("expected single silence row, got %d", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Errorf("silence row length: %d", len(rows[0]))
	}
	if rows[0][0].CallbackData != "s|fp|1h" {
		t.Errorf("callback_data: %s", rows[0][0].CallbackData)
	}
	for _, b := range rows[0] {
		if b.URL != "" {
			t.Errorf("URL button leaked into silence row: %+v", b)
		}
	}
	if _, ok := cache.Get("fp"); !ok {
		t.Error("labels should be cached on keyboard build")
	}
}

func TestKeyboard_SuppressedForResolved(t *testing.T) {
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	if opts := k.Build(
		notification.ChatTarget{ChatID: -100},
		notification.Notification{Status: "resolved", Fingerprint: "fp"},
		"alertmanager",
	); opts != nil {
		t.Error("resolved alerts should not get silence buttons")
	}
}

func TestKeyboard_SuppressedForUnlistedChat(t *testing.T) {
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	if opts := k.Build(
		notification.ChatTarget{ChatID: -999},
		notification.Notification{Status: "firing", Fingerprint: "fp"},
		"alertmanager",
	); opts != nil {
		t.Error("unlisted chat should not get buttons")
	}
}

func TestKeyboard_SuppressedForOtherSource(t *testing.T) {
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	if opts := k.Build(
		notification.ChatTarget{ChatID: -100},
		notification.Notification{Status: "firing", Fingerprint: "fp"},
		"kubewatch",
	); opts != nil {
		t.Error("non-alertmanager source should not get silence buttons")
	}
}

func TestKeyboard_SuppressedWhenFingerprintEmpty(t *testing.T) {
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	if opts := k.Build(
		notification.ChatTarget{ChatID: -100},
		notification.Notification{Status: "firing"},
		"alertmanager",
	); opts != nil {
		t.Error("empty fingerprint should not get buttons")
	}
}
