package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/source"
)

// targetResolver maps a request to its cluster and delivery targets. A
// non-zero status is returned as the HTTP error together with err.
type targetResolver func(r *http.Request) (cluster *Cluster, targets []sink.Target, status int, err error)

type webhookDeps struct {
	source       source.Source
	sinks        map[string]sink.Sink
	readiness    *SinkReadiness
	maxBodyBytes int64
	templateName string
	resolve      targetResolver
	keyboard     KeyboardBuilder
	tracker      ButtonRegistrar
	dedup        *dedup.Cache
	activity     *ActivityTracker
	// resolveMode/messages link resolves to firing messages (resolve.mode);
	// messages is nil for mode "new".
	resolveMode string
	messages    *MessageLog
}

// ButtonRegistrar records sent alert messages so the callback handler can
// validate and the sweeper can expire them.
type ButtonRegistrar interface {
	RegisterRef(cluster string, ref sink.MessageRef, fingerprint string, part sink.Part)
	// ForgetRef drops a message alertly rewrote itself, so the sweeper or a
	// late click does not restore its old body (Slack chat.update resends
	// the stored part).
	ForgetRef(ref sink.MessageRef)
}

// KeyboardBuilder returns the buttons for a given cluster + target +
// notification, or nil if none should be attached. Always safe to return nil.
type KeyboardBuilder interface {
	Build(cluster *Cluster, target sink.Target, n notification.Notification, sourceName string) *sink.Actions
}

// sinkStats is the per-sink outcome of one webhook.
type sinkStats struct {
	Attempts int `json:"attempts"`
	Errors   int `json:"errors"`
	// deadlineHit marks that the request budget ran out before every
	// notification was attempted on this sink.
	deadlineHit bool
}

func webhookHandler(d webhookDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := loggerFrom(ctx).With("source", d.source.Name())
		received := func(cluster string, status int) {
			metrics.NotificationsReceived.WithLabelValues(d.source.Name(), strconv.Itoa(status), cluster).Inc()
		}

		cluster, targets, status, err := d.resolve(r)
		if err != nil {
			clusterName := ""
			if cluster != nil {
				clusterName = cluster.Name
			}
			logger.Warn("webhook target resolution failed", "cluster", clusterName, "status", status, "err", err)
			http.Error(w, err.Error(), status)
			received(clusterName, status)
			return
		}
		logger = logger.With("cluster", cluster.Name)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBodyBytes))
		if err != nil {
			logger.Warn("read body failed", "err", err)
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			received(cluster.Name, http.StatusRequestEntityTooLarge)
			return
		}

		parseStart := time.Now()
		notes, err := d.source.Parse(body)
		metrics.SourceParseDuration.WithLabelValues(d.source.Name()).Observe(time.Since(parseStart).Seconds())
		if err != nil {
			logger.Warn("parse failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			received(cluster.Name, http.StatusBadRequest)
			return
		}
		d.activity.RecordWebhook(d.source.Name())
		for i := range notes {
			notes[i].Cluster = cluster.DisplayName()
		}

		stats := d.deliver(ctx, logger, cluster, targets, notes)

		var totalAttempts, totalErrors int
		deadlineHit := false
		for _, st := range stats {
			totalAttempts += st.Attempts
			totalErrors += st.Errors
			deadlineHit = deadlineHit || st.deadlineHit
		}
		if deadlineHit {
			logger.Warn("request deadline reached; remaining notifications not attempted",
				"attempts", totalAttempts,
				"errors", totalErrors,
				"notifications", len(notes),
			)
		}

		switch {
		case totalAttempts == 0:
			status = http.StatusNoContent
		case totalErrors == 0:
			status = http.StatusOK
		case totalErrors < totalAttempts:
			status = http.StatusMultiStatus
		default:
			status = http.StatusInternalServerError
		}
		if deadlineHit {
			// Part of the payload was never attempted: never answer 200/204,
			// so the caller knows to retry (dedup keeps the retry from
			// duplicating whatever already landed).
			switch status {
			case http.StatusOK, http.StatusNoContent:
				status = http.StatusMultiStatus
			}
		}

		received(cluster.Name, status)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"attempts":%d,"errors":%d,"sinks":%s}`, totalAttempts, totalErrors, sinkStatsJSON(stats))
	}
}

// deliver fans the notifications out to every target. Each sink gets its own
// goroutine working through all notifications for its targets, so a slow or
// failing messenger never eats another one's share of the request budget;
// within a sink the order of notifications and parts is preserved.
func (d webhookDeps) deliver(ctx context.Context, logger *slog.Logger, cluster *Cluster, targets []sink.Target, notes []notification.Notification) map[string]*sinkStats {
	bySink := map[string][]sink.Target{}
	var order []string
	for _, t := range targets {
		if _, seen := bySink[t.Sink]; !seen {
			order = append(order, t.Sink)
		}
		bySink[t.Sink] = append(bySink[t.Sink], t)
	}

	stats := make(map[string]*sinkStats, len(order))
	var wg sync.WaitGroup
	for _, name := range order {
		st := &sinkStats{}
		stats[name] = st
		wg.Add(1)
		go func(name string, st *sinkStats) {
			defer wg.Done()
			d.deliverSink(ctx, logger.With("sink", name), cluster, d.sinks[name], bySink[name], notes, st)
		}(name, st)
	}
	wg.Wait()
	return stats
}

func (d webhookDeps) deliverSink(ctx context.Context, logger *slog.Logger, cluster *Cluster, s sink.Sink, targets []sink.Target, notes []notification.Notification, st *sinkStats) {
	for _, n := range notes {
		if ctx.Err() != nil {
			st.deadlineHit = true
			return
		}
		parts, err := s.Render(d.templateName, n)
		if err != nil {
			logger.Error("render failed", "fingerprint", n.Fingerprint, "err", err)
			metrics.TemplateRenderErrors.WithLabelValues(d.templateName).Inc()
			st.Errors++
			st.Attempts++
			continue
		}

		for _, target := range targets {
			if ctx.Err() != nil {
				st.deadlineHit = true
				return
			}
			if !d.deliverTarget(ctx, logger, cluster, s, target, n, parts, st) {
				st.deadlineHit = true
				return
			}
		}
	}
}

// deliverTarget sends all parts of one notification to one target. It
// returns false when the request budget ran out (the sink must stop: further
// sends would only fail the same way and the caller's retry will deliver
// them, deduped).
func (d webhookDeps) deliverTarget(ctx context.Context, logger *slog.Logger, cluster *Cluster, s sink.Sink, target sink.Target, n notification.Notification, parts []sink.Part, st *sinkStats) bool {
	dedupKey := dedup.Key(n.Fingerprint, cluster.Name, target.String(), n.Status)
	if d.dedup.Reserve(dedupKey) {
		metrics.DedupSkipped.WithLabelValues(d.source.Name(), target.Chat, n.Status, cluster.Name, target.Sink).Inc()
		logger.Info("dedup: skip duplicate delivery",
			"target", target.String(),
			"fingerprint", n.Fingerprint,
			"status", n.Status,
		)
		return true
	}

	linkKey, linkable := d.linkKey(cluster, target, n)
	if linkable && n.Status == "resolved" {
		if msgs, ok := d.messages.Get(linkKey); ok {
			return d.deliverResolve(ctx, logger, cluster, s, target, n, parts, msgs, linkKey, dedupKey, st)
		}
		metrics.ResolveLinked.WithLabelValues(target.Sink, "unlinked").Inc()
	}
	cont, sent := d.sendParts(ctx, logger, cluster, s, target, n, parts, dedupKey, st)
	if linkable && n.Status == "firing" && sent.First.ID != "" {
		d.messages.Add(linkKey, sent)
	}
	return cont
}

// sendParts delivers parts as new messages. sent has First set only when
// every part landed.
func (d webhookDeps) sendParts(ctx context.Context, logger *slog.Logger, cluster *Cluster, s sink.Sink, target sink.Target, n notification.Notification, parts []sink.Part, dedupKey string, st *sinkStats) (cont bool, sent FiringMessage) {
	sentAny, failed := false, false
	for idx, part := range parts {
		st.Attempts++
		// Buttons go on the last part only, so they appear once per
		// (notification, target).
		isLastPart := idx == len(parts)-1
		var actions *sink.Actions
		if isLastPart && d.keyboard != nil {
			actions = d.keyboard.Build(cluster, target, n, d.source.Name())
		}
		ref, err := d.send(ctx, s, target, part, actions)
		if err != nil && s.Classify(err) == sink.ErrCanceled {
			// Budget exhausted (deadline or the limiter refusing to wait past
			// it): not an attempt the messenger saw, not an error to count.
			st.Attempts--
			if !sentAny {
				d.dedup.Forget(dedupKey)
			}
			return false, FiringMessage{}
		}
		if err != nil {
			st.Errors++
			failed = true
			logger.Error("send failed",
				"target", target.String(),
				"fingerprint", n.Fingerprint,
				"err", err,
			)
			metrics.NotificationsSent.WithLabelValues(target.Chat, "error", target.Sink).Inc()
			continue
		}
		sentAny = true
		metrics.NotificationsSent.WithLabelValues(target.Chat, "ok", target.Sink).Inc()
		if idx == 0 {
			sent.First = ref
		}
		if isLastPart {
			sent.Last, sent.LastPart = ref, part
		}
		if isLastPart && actions != nil && d.tracker != nil && ref.ID != "" && ref.ID != "0" {
			d.tracker.RegisterRef(cluster.Name, ref, n.Fingerprint, part)
		}
	}
	// Roll back the reservation only when nothing was delivered: the caller's
	// retry should be allowed to deliver it for real. If at least one part
	// landed, keep it so the retry does not duplicate what is already there.
	if !sentAny && failed {
		d.dedup.Forget(dedupKey)
	}
	if failed || sent.First.ID == "0" {
		return true, FiringMessage{} // partial or dry run: nothing to link
	}
	return true, sent
}

// linkKey reports whether n takes part in resolve linking and its key.
// Only Alertmanager has firing/resolved pairs of one incident.
func (d webhookDeps) linkKey(cluster *Cluster, target sink.Target, n notification.Notification) (string, bool) {
	if d.messages == nil || d.source.Name() != "alertmanager" || n.Fingerprint == "" || n.StartsAt.IsZero() {
		return "", false
	}
	return MessageLogKey(cluster.Name, n.Fingerprint, n.StartsAt, target), true
}

// deliverResolve answers and/or rewrites the firing messages of n's incident.
// reply: the resolve goes out as a reply to the newest firing message (that
// is the delivery), then every firing message is rewritten as resolved
// without buttons (cosmetic: a failure is logged, not counted). edit: the
// rewrite is the delivery; if no firing message could be rewritten the
// resolve falls back to a new message.
func (d webhookDeps) deliverResolve(ctx context.Context, logger *slog.Logger, cluster *Cluster, s sink.Sink, target sink.Target, n notification.Notification, parts []sink.Part, msgs []FiringMessage, linkKey, dedupKey string, st *sinkStats) bool {
	newest := msgs[len(msgs)-1]
	if d.resolveMode == config.ResolveReply {
		reply := target
		reply.ReplyTo = newest.First.ID
		// The firing message itself is rewritten with the full resolve; the
		// reply only has to say "resolved" under it.
		replyParts, err := s.Render(d.templateName, compactResolve(n))
		if err != nil {
			replyParts = parts
		}
		errsBefore := st.Errors
		cont, _ := d.sendParts(ctx, logger, cluster, s, reply, n, replyParts, dedupKey, st)
		if !cont {
			return false
		}
		if st.Errors-errsBefore == len(replyParts) {
			return true // nothing landed: keep the link for the caller's retry
		}
		metrics.ResolveLinked.WithLabelValues(target.Sink, "replied").Inc()
		d.rewrite(ctx, logger, s, target, n, parts[0], msgs)
		d.messages.Delete(linkKey)
		return true
	}

	edited := d.rewrite(ctx, logger, s, target, n, parts[0], msgs)
	if edited > 0 {
		d.messages.Delete(linkKey)
		st.Attempts++
		metrics.ResolveLinked.WithLabelValues(target.Sink, "edited").Inc()
		metrics.NotificationsSent.WithLabelValues(target.Chat, "ok", target.Sink).Inc()
		return true
	}
	if ctx.Err() != nil {
		d.dedup.Forget(dedupKey)
		return false
	}
	d.messages.Delete(linkKey)
	logger.Warn("resolve: firing messages could not be edited, sending a new message",
		"target", target.String(), "fingerprint", n.Fingerprint)
	cont, _ := d.sendParts(ctx, logger, cluster, s, target, n, parts, dedupKey, st)
	return cont
}

// compactResolve keeps the title and status line of a resolve: what a reply
// under the (rewritten) firing message needs.
func compactResolve(n notification.Notification) notification.Notification {
	n.Body = ""
	n.Labels = nil
	n.Links = nil
	return n
}

// rewrite turns every firing message into the resolved one (first part) and
// drops the buttons of the part that carried them. It returns how many
// firing messages were rewritten.
func (d webhookDeps) rewrite(ctx context.Context, logger *slog.Logger, s sink.Sink, target sink.Target, n notification.Notification, resolved sink.Part, msgs []FiringMessage) int {
	edited := 0
	for _, m := range msgs {
		if ctx.Err() != nil {
			break
		}
		if d.tracker != nil {
			d.tracker.ForgetRef(m.Last)
		}
		if err := d.call(s, s.Edit(ctx, m.First, resolved, nil)); err != nil {
			metrics.ResolveLinked.WithLabelValues(target.Sink, "edit_failed").Inc()
			logger.Warn("resolve: edit firing message failed",
				"target", target.String(), "fingerprint", n.Fingerprint, "message", m.First.ID, "err", err)
			continue
		}
		edited++
		if m.Last != m.First {
			if err := d.call(s, s.SetActions(ctx, m.Last, m.LastPart, nil)); err != nil {
				logger.Warn("resolve: remove buttons failed",
					"target", target.String(), "message", m.Last.ID, "err", err)
			}
		}
	}
	return edited
}

// call records a messenger call outcome in the sink's readiness, like send.
func (d webhookDeps) call(s sink.Sink, err error) error {
	tracker := d.readiness.Get(s.Name())
	if tracker == nil {
		return err
	}
	if err == nil {
		tracker.RecordSendSuccess()
	} else {
		tracker.RecordSendFailure(s.Classify(err) == sink.ErrServer)
	}
	return err
}

func (d webhookDeps) send(ctx context.Context, s sink.Sink, t sink.Target, p sink.Part, actions *sink.Actions) (sink.MessageRef, error) {
	ref, err := s.Send(ctx, t, p, actions)
	tracker := d.readiness.Get(s.Name())
	if err == nil {
		if tracker != nil {
			tracker.RecordSendSuccess()
		}
		d.activity.RecordDelivery()
		return ref, nil
	}
	// Server-side degradation (5xx, network) counts toward unreadiness; 4xx,
	// 429 (backpressure) and our own cancellations do not.
	if tracker != nil {
		tracker.RecordSendFailure(s.Classify(err) == sink.ErrServer)
	}
	return sink.MessageRef{}, err
}

func sinkStatsJSON(stats map[string]*sinkStats) string {
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%q:{"attempts":%d,"errors":%d}`, n, stats[n].Attempts, stats[n].Errors)
	}
	b.WriteByte('}')
	return b.String()
}

// legacyResolver serves /v1/{source}/{chats}: targets come from the URL and
// must pass the sink allowlists; the cluster is the default one.
func legacyResolver(cluster *Cluster, sinks map[string]sink.Sink, tgAllow []int64, slackAllow []string) targetResolver {
	return func(r *http.Request) (*Cluster, []sink.Target, int, error) {
		targets, err := parseChatTargets(r.PathValue("chats"))
		if err != nil {
			return cluster, nil, http.StatusBadRequest, err
		}
		for _, t := range targets {
			if _, ok := sinks[t.Sink]; !ok {
				return cluster, nil, http.StatusBadRequest, fmt.Errorf("sink %q is not enabled", t.Sink)
			}
			switch t.Sink {
			case sink.Telegram:
				if len(tgAllow) > 0 {
					id, _ := strconv.ParseInt(t.Chat, 10, 64)
					if !int64InSet(id, tgAllow) {
						return cluster, nil, http.StatusForbidden, fmt.Errorf("chat %s is not in telegram.chat_allowlist", t.Chat)
					}
				}
			case sink.Slack:
				if len(slackAllow) > 0 && !stringInSet(t.Chat, slackAllow) {
					return cluster, nil, http.StatusForbidden, fmt.Errorf("channel %s is not in slack.channel_allowlist", t.Chat)
				}
			}
		}
		return cluster, targets, 0, nil
	}
}

// clusterResolver serves /v1/clusters/{cluster}/{source}/{destination}. The
// cluster itself was already authenticated by clusterAuthMiddleware.
func clusterResolver(clusters map[string]*Cluster) targetResolver {
	return func(r *http.Request) (*Cluster, []sink.Target, int, error) {
		cluster := clusters[r.PathValue("cluster")]
		if cluster == nil {
			return nil, nil, http.StatusUnauthorized, errors.New("unauthorized")
		}
		name := r.PathValue("destination")
		targets, ok := cluster.Destinations[name]
		if !ok {
			return cluster, nil, http.StatusNotFound, fmt.Errorf("unknown destination %q", name)
		}
		return cluster, targets, 0, nil
	}
}

func stringInSet(v string, set []string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}
