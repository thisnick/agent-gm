package libgm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

type passiveTransport func(*http.Request) (*http.Response, error)

func (f passiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type passiveBody struct {
	ctx    context.Context
	opened bool
	closed chan struct{}
	once   atomic.Bool
}

func (b *passiveBody) Read(out []byte) (int, error) {
	if !b.opened {
		b.opened = true
		return copy(out, "[["), nil
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.EOF
	}
}
func (b *passiveBody) Close() error {
	if b.once.CompareAndSwap(false, true) {
		close(b.closed)
	}
	return nil
}

func TestPassiveCancellationNeverClaimsActiveSession(t *testing.T) {
	c := NewClient(&AuthData{TachyonAuthToken: []byte{1}, TachyonExpiry: time.Now().Add(24 * time.Hour), Browser: &gmproto.Device{}}, nil, zerolog.Nop(), exhttp.SensibleClientSettings)
	var calls atomic.Int32
	transport := passiveTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/ReceiveMessages") {
			t.Errorf("unexpected network request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &passiveBody{ctx: r.Context(), closed: make(chan struct{})}}, nil
	})
	c.http = &http.Client{Transport: transport}
	c.lphttp = &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.RunBackground(ctx, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected requests: %d", calls.Load())
	}
	if c.CurrentSessionID() == "" {
		t.Fatal("missing RPC session identifier")
	}
}
