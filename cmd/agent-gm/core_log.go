package main

import "github.com/rs/zerolog"

type coreLog struct{ log zerolog.Logger }

func (l coreLog) emit(event *zerolog.Event, message string, fields ...any) {
	for i := 0; i+1 < len(fields); i += 2 {
		if key, ok := fields[i].(string); ok {
			event = event.Any(key, fields[i+1])
		}
	}
	event.Msg(message)
}
func (l coreLog) Info(message string, fields ...any)  { l.emit(l.log.Info(), message, fields...) }
func (l coreLog) Warn(message string, fields ...any)  { l.emit(l.log.Warn(), message, fields...) }
func (l coreLog) Debug(message string, fields ...any) { l.emit(l.log.Debug(), message, fields...) }
