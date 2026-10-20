package httpapi

import "log/slog"

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
