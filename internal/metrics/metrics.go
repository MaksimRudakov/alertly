package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var (
	registry     *prometheus.Registry
	registerOnce sync.Once

	NotificationsReceived *prometheus.CounterVec
	NotificationsSent     *prometheus.CounterVec
	TelegramAPIDuration   prometheus.Histogram
	TelegramRetries       *prometheus.CounterVec
	TelegramRateLimited   *prometheus.CounterVec
	TemplateRenderErrors  *prometheus.CounterVec
	MessageSplitTotal     prometheus.Counter
	AuthFailures          prometheus.Counter
	SourceParseDuration   *prometheus.HistogramVec
	BuildInfo             *prometheus.GaugeVec
)

func Init() *prometheus.Registry {
	registerOnce.Do(func() {
		registry = prometheus.NewRegistry()

		NotificationsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alertly_notifications_received_total",
			Help: "Number of webhook notifications received per source and resulting status.",
		}, []string{"source", "status_code"})

		NotificationsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alertly_notifications_sent_total",
			Help: "Number of telegram messages sent per chat and outcome.",
		}, []string{"chat_id", "status"})

		TelegramAPIDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "alertly_telegram_api_duration_seconds",
			Help:    "Duration of Telegram Bot API requests.",
			Buckets: prometheus.DefBuckets,
		})

		TelegramRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alertly_telegram_retries_total",
			Help: "Number of retries against Telegram Bot API.",
		}, []string{"reason"})

		TelegramRateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alertly_telegram_rate_limited_total",
			Help: "Number of times a chat was rate limited locally.",
		}, []string{"chat_id"})

		TemplateRenderErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "alertly_template_render_errors_total",
			Help: "Number of template render failures.",
		}, []string{"template"})

		MessageSplitTotal = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "alertly_message_split_total",
			Help: "Number of times a message had to be split due to length.",
		})

		AuthFailures = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "alertly_auth_failures_total",
			Help: "Number of failed bearer auth attempts.",
		})

		SourceParseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "alertly_source_parse_duration_seconds",
			Help:    "Time spent parsing webhook payloads per source.",
			Buckets: prometheus.DefBuckets,
		}, []string{"source"})

		BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "alertly_build_info",
			Help: "Build metadata. Always 1.",
		}, []string{"version", "commit", "go_version"})

		registry.MustRegister(
			NotificationsReceived,
			NotificationsSent,
			TelegramAPIDuration,
			TelegramRetries,
			TelegramRateLimited,
			TemplateRenderErrors,
			MessageSplitTotal,
			AuthFailures,
			SourceParseDuration,
			BuildInfo,
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
	})
	return registry
}

func Registry() *prometheus.Registry { return registry }

func ChatLabel(chatID int64) string { return strconv.FormatInt(chatID, 10) }
