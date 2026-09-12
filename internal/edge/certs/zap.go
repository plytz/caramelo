package certs

import (
	"bytes"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newLogger(log LogFunc) *zap.Logger {
	if log == nil {
		return zap.NewNop()
	}
	enc := zap.NewProductionEncoderConfig()
	enc.TimeKey = ""
	return zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(enc),
		zapcore.AddSync(&logWriter{log: log}),
		zap.InfoLevel,
	))
}

type logWriter struct{ log LogFunc }

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		w.log("%s", line)
	}
	return len(p), nil
}
