package source

import "github.com/MaksimRudakov/alertly/internal/notification"

type Source interface {
	Name() string
	Parse(body []byte) ([]notification.Notification, error)
}
