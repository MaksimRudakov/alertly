package server

import (
	"context"
	"log/slog"
)

func contextWithRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, rid)
}

func contextWithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, logger)
}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
