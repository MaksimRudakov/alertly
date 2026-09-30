package server

import (
	"strings"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
)

// testCluster is the implicit default cluster with an Alertmanager, i.e. the
// single-cluster setup every pre-cluster test assumed.
var testCluster = &Cluster{Name: config.DefaultCluster, Alias: config.DefaultCluster, Implicit: true, AM: &fakeAM{}}

func tgTarget(chat string) sink.Target { return sink.Target{Sink: sink.Telegram, Chat: chat} }

func TestKeyboard_FiringAllowlistedChat(t *testing.T) {
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h", "4h"},
		ChatAllowlist: []int64{-100},
		Cache:         cache,
	}
	opts := k.Build(
		testCluster, tgTarget("-100"),
		notification.Notification{
			Status:      "firing",
			Fingerprint: "fp",
			Labels:      map[string]string{"a": "1"},
			Links:       []notification.Link{{Title: "Runbook", URL: "https://rb"}},
		},
		"alertmanager",
	)
	if opts == nil {
		t.Fatal("expected keyboard")
	}
	rows := opts.Rows
	if len(rows) != 1 {
		t.Fatalf("expected single silence row, got %d", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Errorf("silence row length: %d", len(rows[0]))
	}
	if rows[0][0].Data != "s|fp|1h" {
		t.Errorf("callback_data: %s", rows[0][0].Data)
	}
	if _, ok := cache.Get("fp"); !ok {
		t.Error("labels should be cached on keyboard build")
	}
}

func TestKeyboard_SkipsOversizedCallbackData(t *testing.T) {
	longFP := strings.Repeat("f", 80) // "s|<80 chars>|1h" > 64 bytes
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	opts := k.Build(
		testCluster, tgTarget("-100"),
		notification.Notification{Status: "firing", Fingerprint: longFP, Labels: map[string]string{"a": "1"}},
		"alertmanager",
	)
	if opts != nil {
		t.Error("keyboard with only oversized callback_data buttons should be dropped entirely")
	}

	// A normal fingerprint on the same builder still yields buttons.
	opts = k.Build(
		testCluster, tgTarget("-100"),
		notification.Notification{Status: "firing", Fingerprint: "fp", Labels: map[string]string{"a": "1"}},
		"alertmanager",
	)
	if opts == nil {
		t.Fatal("expected keyboard for normal fingerprint")
	}
	for _, b := range opts.Rows[0] {
		if len(b.Data) > maxCallbackDataBytes {
			t.Errorf("callback_data exceeds limit: %d bytes", len(b.Data))
		}
	}
}

func TestKeyboard_SuppressedForResolved(t *testing.T) {
	k := &AlertmanagerKeyboard{
		Durations:     []string{"1h"},
		ChatAllowlist: []int64{-100},
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
	}
	if opts := k.Build(
		testCluster, tgTarget("-100"),
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
		testCluster, tgTarget("-999"),
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
		testCluster, tgTarget("-100"),
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
		testCluster, tgTarget("-100"),
		notification.Notification{Status: "firing"},
		"alertmanager",
	); opts != nil {
		t.Error("empty fingerprint should not get buttons")
	}
}

func TestKeyboard_NamedClusterCarriesAlias(t *testing.T) {
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	k := &AlertmanagerKeyboard{Durations: []string{"1h"}, ChatAllowlist: []int64{-100}, Cache: cache}
	prod := &Cluster{Name: "k8s-prod", Alias: "prod", AM: &fakeAM{}}

	opts := k.Build(prod, tgTarget("-100"),
		notification.Notification{Status: "firing", Fingerprint: "fp", Labels: map[string]string{"a": "1"}},
		"alertmanager")
	if opts == nil || opts.Rows[0][0].Data != "s|prod|fp|1h" {
		t.Fatalf("named cluster callback data: %+v", opts)
	}
	if _, ok := cache.Get("k8s-prod|fp"); !ok {
		t.Error("labels must be cached under the cluster-scoped key")
	}
	if _, ok := cache.Get("fp"); ok {
		t.Error("named cluster must not write the default-cluster key")
	}
}

func TestKeyboard_SuppressedWithoutClusterAM(t *testing.T) {
	k := &AlertmanagerKeyboard{Durations: []string{"1h"}, ChatAllowlist: []int64{-100}, Cache: alertmanager.NewLabelCache(time.Hour, 10)}
	noAM := &Cluster{Name: "edge", Alias: "edge"}
	if opts := k.Build(noAM, tgTarget("-100"),
		notification.Notification{Status: "firing", Fingerprint: "fp"}, "alertmanager"); opts != nil {
		t.Error("cluster without an Alertmanager must not get silence buttons")
	}
}

func TestKeyboard_SuppressedForSlackTarget(t *testing.T) {
	k := &AlertmanagerKeyboard{Durations: []string{"1h"}, ChatAllowlist: []int64{-100}, Cache: alertmanager.NewLabelCache(time.Hour, 10)}
	if opts := k.Build(testCluster, sink.Target{Sink: sink.Slack, Chat: "C0123ABCD"},
		notification.Notification{Status: "firing", Fingerprint: "fp"}, "alertmanager"); opts != nil {
		t.Error("slack targets get no buttons until Slack interactivity ships")
	}
}

func TestKeyboard_SlackChannelAllowlisted(t *testing.T) {
	k := &AlertmanagerKeyboard{Durations: []string{"1h"}, SlackChannels: []string{"C0123ABCD"}, Cache: alertmanager.NewLabelCache(time.Hour, 10)}
	prod := &Cluster{Name: "k8s-prod", Alias: "prod", AM: &fakeAM{}}
	n := notification.Notification{Status: "firing", Fingerprint: "fp", Labels: map[string]string{"a": "1"}}
	if a := k.Build(prod, sink.Target{Sink: sink.Slack, Chat: "C0123ABCD"}, n, "alertmanager"); a == nil || a.Rows[0][0].Data != "s|prod|fp|1h" {
		t.Fatalf("slack buttons: %+v", a)
	}
	if a := k.Build(prod, sink.Target{Sink: sink.Slack, Chat: "C0OTHER01"}, n, "alertmanager"); a != nil {
		t.Error("unlisted slack channel must not get buttons")
	}
	if a := k.Build(prod, tgTarget("-100"), n, "alertmanager"); a != nil {
		t.Error("telegram chat outside ChatAllowlist must not get buttons")
	}
}
