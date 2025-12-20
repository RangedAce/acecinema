package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New builds a structured logger with RFC3339 timestamps.
func New() zerolog.Logger {
	return zerolog.New(os.Stdout).With().Timestamp().Logger().Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	})
}
