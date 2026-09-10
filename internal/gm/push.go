package gm

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// PushSubscription is stored in an encrypted sidecar next to AuthData.
// Generation/Delivered make acknowledged HTTP wakes survive a process crash.
type PushSubscription struct {
	Private         []byte `json:"private"`
	Auth            []byte `json:"auth"`
	Token           string `json:"token"`
	Generation      uint64 `json:"generation"`
	Delivered       uint64 `json:"delivered"`
	LastReceivedMS  int64  `json:"last_received_ms,omitempty"`
	LastCompletedMS int64  `json:"last_completed_ms,omitempty"`
}

type pushRuntime struct {
	run      func(context.Context, func(context.Context) error) error
	mu       sync.Mutex
	state    PushSubscription
	save     func([]byte) error
	saveAuth func([]byte) error
	endpoint string
	gate     chan struct{}
	wake     chan struct{}
	started  atomic.Bool
	life     sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

// EnablePush configures push before Connect. It never connects on its own.
func (b *LibGM) EnablePush(endpointBase string, saved []byte, save, saveAuth func([]byte) error) error {
	if b.push != nil {
		return nil
	}
	u, err := url.Parse(endpointBase)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("push requires a public HTTPS endpoint")
	}
	p := &pushRuntime{save: save, saveAuth: saveAuth, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	p.run = func(ctx context.Context, request func(context.Context) error) error {
		return b.client.RunBackground(ctx, request)
	}
	if len(saved) > 0 {
		if err = json.Unmarshal(saved, &p.state); err != nil {
			return errors.New("invalid saved push subscription")
		}
	} else {
		key, e := ecdh.P256().GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		p.state.Private = key.Bytes()
		p.state.Auth = make([]byte, 16)
		if _, err = rand.Read(p.state.Auth); err != nil {
			return err
		}
		token := make([]byte, 32)
		if _, err = rand.Read(token); err != nil {
			return err
		}
		p.state.Token = hex.EncodeToString(token)
	}
	if _, err = ecdh.P256().NewPrivateKey(p.state.Private); err != nil {
		return errors.New("invalid push private key")
	}
	if token, e := hex.DecodeString(p.state.Token); e != nil || len(token) != 32 || len(p.state.Auth) != 16 || p.state.Delivered > p.state.Generation {
		return errors.New("invalid push subscription")
	}
	p.endpoint = strings.TrimRight(endpointBase, "/") + "/" + p.state.Token
	if err = p.persistLocked(); err != nil {
		return err
	}
	b.push = p
	return nil
}

func (p *pushRuntime) persistLocked() error {
	data, err := json.Marshal(p.state)
	if err != nil {
		return err
	}
	return p.save(data)
}

func (b *LibGM) connectPush(ctx context.Context) error {
	p := b.push
	p.life.Lock()
	if p.started.Load() {
		p.life.Unlock()
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.done = make(chan struct{})
	p.started.Store(true)
	p.life.Unlock()
	err := b.withPush(ctx, func(ctx context.Context) error {
		key, e := ecdh.P256().NewPrivateKey(p.state.Private)
		if e != nil {
			return e
		}
		return b.RegisterWebPush(ctx, p.endpoint, key.PublicKey().Bytes(), p.state.Auth)
	})
	if err != nil {
		p.started.Store(false)
		p.cancel()
		close(p.done)
		return err
	}
	go b.pushWorker()
	p.mu.Lock()
	pending := p.state.Generation > p.state.Delivered
	p.mu.Unlock()
	if pending {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	b.pushInfo("push connection ready")
	return nil
}

func (b *LibGM) disconnectPush() {
	p := b.push
	p.life.Lock()
	p.mu.Lock()
	wasStarted := p.started.Swap(false)
	p.mu.Unlock()
	if !wasStarted {
		p.life.Unlock()
		return
	}
	p.cancel()
	done := p.done
	p.life.Unlock()
	<-done
	// Wait for any in-flight API batch to release the client as well.
	p.gate <- struct{}{}
	<-p.gate
}

// withPush serializes all protocol access. An idle account has no Google
// listener. API requests, authenticated pushes, and catch-up sweeps wake it.
type passiveSessionKey struct{}

// WithSession lets a serialized refresh or mutation reuse one passive listener.
// The callback must issue protocol calls sequentially and not retain its context.
func (b *LibGM) WithSession(ctx context.Context, request func(context.Context) error) error {
	return b.withPush(ctx, func(ctx context.Context) error {
		return request(context.WithValue(ctx, passiveSessionKey{}, b))
	})
}

func (b *LibGM) withPush(ctx context.Context, request func(context.Context) error) error {
	if ctx.Value(passiveSessionKey{}) == b {
		return request(ctx)
	}

	p := b.push
	if p == nil {
		return request(ctx)
	}
	if !p.started.Load() {
		return errors.New("push client is stopped")
	}
	p.life.Lock()
	lifeCtx := p.ctx
	p.life.Unlock()
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-lifeCtx.Done():
		return lifeCtx.Err()
	}
	defer func() { <-p.gate }()
	if lifeCtx.Err() != nil {
		return lifeCtx.Err()
	}
	batchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	stop := context.AfterFunc(lifeCtx, cancel)
	defer func() { stop(); cancel() }()
	err := p.run(batchCtx, request)
	data, saveErr := b.marshalSession()
	if saveErr == nil {
		saveErr = p.saveAuth(data)
	}
	if saveErr != nil {
		return fmt.Errorf("saving push session: %w", saveErr)
	}
	return err
}

func (b *LibGM) pushWorker() {
	p := b.push
	defer close(p.done)
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		}
		backoff := time.Second
		for {
			p.mu.Lock()
			generation := p.state.Generation
			pending := generation > p.state.Delivered
			p.mu.Unlock()
			if !pending {
				break
			}
			err := b.withPush(p.ctx, func(context.Context) error { return nil })
			if err == nil {
				p.mu.Lock()
				old := p.state.Delivered
				oldCompleted := p.state.LastCompletedMS
				p.state.LastCompletedMS = time.Now().UnixMilli()
				p.state.Delivered = generation
				err = p.persistLocked()
				if err != nil {
					p.state.Delivered = old
					p.state.LastCompletedMS = oldCompleted
				}
				p.mu.Unlock()
			}
			if err == nil {
				b.pushInfo("push fetch completed")
				backoff = time.Second
				continue
			}
			if p.ctx.Err() != nil {
				return
			}
			b.log.Warn().Msg("Push fetch failed; retrying the pending wake")
			timer := time.NewTimer(backoff)
			select {
			case <-p.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, 5*time.Minute)
		}
	}
}

// HandlePush verifies encryption before durably accepting a wake. The random
// capability is never logged. It does not forward any decrypted payload.
func (b *LibGM) HandlePush(w http.ResponseWriter, r *http.Request, token string) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	p := b.push
	if p == nil || !p.started.Load() || subtle.ConstantTimeCompare([]byte(token), []byte(p.state.Token)) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		http.Error(w, "invalid push", http.StatusBadRequest)
		return
	}
	switch r.Header.Get("Content-Encoding") {
	case "aesgcm":
		_, err = DecryptLegacyWebPush(p.state.Private, p.state.Auth, body, r.Header.Get("Encryption"), r.Header.Get("Crypto-Key"))
	case "aes128gcm":
		_, err = DecryptWebPush(p.state.Private, p.state.Auth, body)
	default:
		http.Error(w, "unsupported encoding", http.StatusUnsupportedMediaType)
		return
	}
	if err != nil {
		http.Error(w, "invalid push", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if !p.started.Load() {
		p.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	old := p.state.Generation
	oldReceived := p.state.LastReceivedMS
	p.state.LastReceivedMS = time.Now().UnixMilli()
	p.state.Generation++
	err = p.persistLocked()
	if err != nil {
		p.state.Generation = old
		p.state.LastReceivedMS = oldReceived
	}
	p.mu.Unlock()
	if err != nil {
		http.Error(w, "push storage unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
	b.pushInfo("push received")
	w.WriteHeader(http.StatusCreated)
}

// SetPushPairingMode prevents the upstream post-pair active reconnect. The
// supervisor configures the account's durable push endpoint after pairing.
func (b *LibGM) SetPushPairingMode() { b.client.DisablePostPairConnect = true }

func (b *LibGM) SetPushLogger(log zerolog.Logger) { b.pushLog = &log }
func (b *LibGM) pushInfo(message string) {
	if b.pushLog != nil {
		b.pushLog.Info().Msg(message)
	}
}
