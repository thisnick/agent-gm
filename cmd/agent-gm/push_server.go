package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/thisnick/agent-gm/internal/accounts"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

func configureServerPush(sup *accounts.Supervisor, sessions *store.SessionStore, publicURL string, log zerolog.Logger) {
	// Keep the supervisor's 15-minute reconciliation timer in push mode.
	sup.PrepareBackend = func(id string, backend gm.Backend) error {
		b, ok := backend.(*gm.LibGM)
		if !ok {
			return nil
		}
		b.SetPushLogger(log.With().Str("account_id", id).Logger())
		saved, err := sessions.Load(id + "-push")
		if err != nil && !errors.Is(err, store.ErrNoSession) {
			return err
		}
		return b.EnablePush(strings.TrimRight(publicURL, "/")+"/push/"+id, saved,
			func(data []byte) error { return sessions.Save(id+"-push", data) },
			func(data []byte) error { return sessions.Save(id, data) })
	}
}

func mountPush(next http.Handler, sup *accounts.Supervisor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/push/") {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/push/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		a, err := sup.Get(parts[0])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		b, ok := a.Backend.(*gm.LibGM)
		if !ok {
			http.NotFound(w, r)
			return
		}
		b.HandlePush(w, r, parts[1])
	})
}
