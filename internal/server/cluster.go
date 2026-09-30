package server

import (
	"strings"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/sink"
)

// Cluster is the runtime view of one configured cluster.
type Cluster struct {
	Name  string
	Alias string
	// Implicit marks the default cluster of legacy single-cluster mode; its
	// name is not shown in messages.
	Implicit     bool
	Destinations map[string][]sink.Target
	// AM serves silence buttons and /status for this cluster; nil = none.
	AM            alertmanager.Client
	WatchdogAlert string
}

// DisplayName is the cluster name as rendered into messages ("" for the
// implicit default, matching the pre-cluster output).
func (c *Cluster) DisplayName() string {
	if c == nil || c.Implicit {
		return ""
	}
	return c.Name
}

// TargetsFromConfig converts a configured destination into sink targets.
func TargetsFromConfig(specs []config.TargetSpec) []sink.Target {
	out := make([]sink.Target, 0, len(specs))
	for _, s := range specs {
		name, addr := s.Sink()
		chat, thread, _ := strings.Cut(addr, ":")
		out = append(out, sink.Target{Sink: name, Chat: chat, Thread: thread})
	}
	return out
}
