package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
)

const (
	resolvePath  = "/v1/clusters/k8s-prod/alertmanager/default"
	resolveToken = "tok-prod"
)

func amAlert(status, startsAt string) string {
	return `{"alerts":[{"status":"` + status + `","fingerprint":"fp1","startsAt":"` + startsAt + `","endsAt":"2026-10-19T10:12:00Z","labels":{"alertname":"X"},"annotations":{"summary":"Disk full"}}]}`
}

const started = "2026-10-19T10:00:00Z"

func withResolve(mode string, log *MessageLog, tracker ButtonRegistrar) func(*Deps) {
	return func(d *Deps) {
		d.ResolveMode = mode
		d.Messages = log
		d.Tracker = tracker
	}
}

type forgetRecorder struct{ forgot []sink.MessageRef }

func (f *forgetRecorder) RegisterRef(string, sink.MessageRef, string, sink.Part) {}
func (f *forgetRecorder) ForgetRef(ref sink.MessageRef)                          { f.forgot = append(f.forgot, ref) }

func mustStatus(t *testing.T, e *clusterEnv, body string, want int) {
	t.Helper()
	resp, b := e.post(t, resolvePath, resolveToken, body)
	if resp.StatusCode != want {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, b)
	}
}

func TestResolve_ReplyAnswersAndRewritesFiringMessage(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	tr := &forgetRecorder{}
	e := newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveReply, log, tr))

	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("resolved", started), http.StatusOK)

	sent := e.tg.Sent()
	if len(sent) != 2 {
		t.Fatalf("want firing + reply, got %d sends", len(sent))
	}
	if sent[0].Target.ReplyTo != "" || sent[1].Target.ReplyTo != "1" {
		t.Errorf("resolve must reply to the firing message 1: %+v", sent)
	}
	if got := compactResolve(notificationWithBody()); got.Body != "" || got.Labels != nil || got.Links != nil || got.Title == "" {
		t.Errorf("the reply keeps only title and status: %+v", got)
	}
	edits := e.tg.Edits()
	if len(edits) != 1 || edits[0].Ref.ID != "1" || edits[0].Actions != nil {
		t.Fatalf("firing message must be rewritten without buttons: %+v", edits)
	}
	if len(tr.forgot) != 1 || tr.forgot[0].ID != "1" {
		t.Errorf("button trackers must forget the rewritten message: %+v", tr.forgot)
	}
	if log.Len() != 0 {
		t.Errorf("link must be dropped after the resolve, %d left", log.Len())
	}
}

func TestResolve_EditOnlyRewrites(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	e := newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveEdit, log, nil))

	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	resp, body := e.post(t, resolvePath, resolveToken, amAlert("resolved", started))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"attempts":1,"errors":0`) {
		t.Fatalf("edit is the delivery: %d %s", resp.StatusCode, body)
	}
	if n := len(e.tg.Sent()); n != 1 {
		t.Errorf("edit mode must not send a new message, got %d sends", n)
	}
	if edits := e.tg.Edits(); len(edits) != 1 || edits[0].Ref.ID != "1" {
		t.Errorf("edits: %+v", edits)
	}
}

func TestResolve_EditFailureFallsBackToNewMessage(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	e := newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveEdit, log, nil))
	e.tg.editFail = errors.New("message to edit not found")

	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("resolved", started), http.StatusOK)
	sent := e.tg.Sent()
	if len(sent) != 2 || sent[1].Target.ReplyTo != "" {
		t.Fatalf("resolve must fall back to a plain new message: %+v", sent)
	}
}

func TestResolve_UnlinkedResolveIsANewMessage(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	e := newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveReply, log, nil))

	// Another incident of the same fingerprint (different startsAt) and a
	// resolve without a known firing message (restart).
	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("resolved", "2026-10-19T09:00:00Z"), http.StatusOK)

	sent := e.tg.Sent()
	if len(sent) != 2 || sent[1].Target.ReplyTo != "" || len(e.tg.Edits()) != 0 {
		t.Fatalf("unlinked resolve must be sent as before: %+v edits=%d", sent, len(e.tg.Edits()))
	}
	if log.Len() != 1 {
		t.Errorf("the firing incident stays linked, got %d", log.Len())
	}
}

func TestResolve_RepeatedFiringMessagesAllRewritten(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	// No dedup: every firing redelivery is a new message, like
	// repeat_interval past the dedup TTL.
	e := newClusterEnv(t, nil, withResolve(config.ResolveReply, log, nil))

	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("resolved", started), http.StatusOK)

	sent := e.tg.Sent()
	if len(sent) != 3 || sent[2].Target.ReplyTo != "2" {
		t.Fatalf("resolve must answer the newest firing message: %+v", sent)
	}
	if edits := e.tg.Edits(); len(edits) != 2 {
		t.Errorf("both firing messages must be rewritten: %+v", edits)
	}
}

func TestResolve_FailedReplyKeepsLink(t *testing.T) {
	log := NewMessageLog(time.Hour, 100)
	e := newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveReply, log, nil))

	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	e.tg.fail = errors.New("boom")
	mustStatus(t, e, amAlert("resolved", started), http.StatusInternalServerError)
	if log.Len() != 1 || len(e.tg.Edits()) != 0 {
		t.Fatalf("a failed reply must keep the link and leave the firing message: links=%d edits=%d", log.Len(), len(e.tg.Edits()))
	}
	e.tg.fail = nil
	mustStatus(t, e, amAlert("resolved", started), http.StatusOK)
	if sent := e.tg.Sent(); sent[len(sent)-1].Target.ReplyTo != "1" {
		t.Errorf("the caller's retry must still reply: %+v", sent)
	}
}

func TestResolve_ModeNewAndOtherSourcesAreUnlinked(t *testing.T) {
	e := newClusterEnv(t, dedup.New(time.Hour))
	mustStatus(t, e, amAlert("firing", started), http.StatusOK)
	mustStatus(t, e, amAlert("resolved", started), http.StatusOK)
	if sent := e.tg.Sent(); len(sent) != 2 || sent[1].Target.ReplyTo != "" || len(e.tg.Edits()) != 0 {
		t.Fatalf("mode new must keep 0.8 behaviour: %+v", sent)
	}

	log := NewMessageLog(time.Hour, 100)
	e = newClusterEnv(t, dedup.New(time.Hour), withResolve(config.ResolveReply, log, nil))
	resp, body := e.post(t, "/v1/clusters/k8s-prod/generic/default", resolveToken, `{"title":"Deploy","status":"firing"}`)
	if resp.StatusCode != http.StatusOK || log.Len() != 0 {
		t.Errorf("only alertmanager alerts are linked: %d %s links=%d", resp.StatusCode, body, log.Len())
	}
}

func TestMessageLog_TTLAndBounds(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewMessageLog(time.Minute, 2)
	l.now = func() time.Time { return now }
	m := func(id string) FiringMessage { return FiringMessage{First: sink.MessageRef{ID: id}} }

	l.Add("a", m("1"))
	l.Add("b", m("2"))
	l.Add("c", m("3"))
	if _, ok := l.Get("a"); ok || l.Len() != 2 {
		t.Fatalf("oldest entry must be evicted beyond max_entries, len=%d", l.Len())
	}

	for i := 0; i < maxMessagesPerAlert+2; i++ {
		l.Add("b", m(string(rune('a'+i))))
	}
	msgs, _ := l.Get("b")
	if len(msgs) != maxMessagesPerAlert || msgs[len(msgs)-1].First.ID != string(rune('a'+maxMessagesPerAlert+1)) {
		t.Errorf("per-incident messages must be capped, newest kept: %+v", msgs)
	}

	now = now.Add(2 * time.Minute)
	if _, ok := l.Get("c"); ok {
		t.Error("expired entry must not be returned")
	}
}

func notificationWithBody() notification.Notification {
	return notification.Notification{Title: "T", Body: "B", Status: "resolved",
		Labels: map[string]string{"a": "b"}, Links: []notification.Link{{Title: "R", URL: "u"}}}
}
