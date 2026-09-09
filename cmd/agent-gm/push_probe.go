package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/config"
	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/logging"
	"github.com/thisnick/agent-gm/internal/store"
)

// push-probe verifies whether Google delivers directly to our HTTPS endpoint.
// It is a temporary diagnostic, with only a random capability URL exposed.
func spikePushProbe(args []string) int {
	fs := flag.NewFlagSet("push-probe", flag.ContinueOnError)
	account := fs.String("account", "", "saved account ID")
	initialFetch := fs.Bool("initial-fetch", false, "perform one background startup sync")
	bootstrapActive := fs.Bool("bootstrap-active", false, "briefly initialize the session before registering push")
	sendTo := fs.String("send-test-to", "", "send one diagnostic SMS to this test number")
	if fs.Parse(args) != nil || *account == "" || strings.ContainsAny(*account, "/\\.") {
		return exitUsage
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	publicURL, err := url.Parse(os.Getenv("AGENT_GM_PUBLIC_URL"))
	if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return fail(errors.New("push-probe requires an HTTPS AGENT_GM_PUBLIC_URL without credentials, query or fragment"))
	}
	if cfg.Backend != config.BackendLibGM || cfg.UnsafeTrace {
		return fail(errors.New("push-probe requires libgm with unsafe tracing disabled"))
	}
	ss, err := store.NewSessionStore(cfg.DataDir, cfg.DataKey)
	if err != nil {
		return fail(err)
	}
	data, err := ss.Load(*account)
	if err != nil {
		return fail(err)
	}
	opts := logging.Options{Level: "warn", Format: "json", Quiet: true}
	b, err := gm.NewFromSession(data, logging.Library(logging.New(opts), opts))
	if err != nil {
		return fail(err)
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fail(err)
	}
	auth := make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		return fail(err)
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return fail(err)
	}
	path := "/push/" + hex.EncodeToString(token)
	subscription := probeSubscription{Private: key.Bytes(), Auth: auth, Path: path}
	// Keep the endpoint stable across probe restarts, sealed with the data key.
	saved, loadErr := ss.Load(*account + "-push-probe")
	if loadErr == nil {
		if err = json.Unmarshal(saved, &subscription); err != nil {
			return fail(err)
		}
		key, err = ecdh.P256().NewPrivateKey(subscription.Private)
		if err != nil || len(subscription.Auth) != 16 || !strings.HasPrefix(subscription.Path, "/push/") || len(subscription.Path) != 70 {
			return fail(errors.New("invalid saved push subscription"))
		}
		auth, path = subscription.Auth, subscription.Path
	} else if errors.Is(loadErr, store.ErrNoSession) {
		saved, err = json.Marshal(subscription)
		if err != nil {
			return fail(err)
		}
		if err = ss.Save(*account+"-push-probe", saved); err != nil {
			return fail(err)
		}
	} else {
		return fail(loadErr)
	}
	wake := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST "+path, probePushHandler(key.Bytes(), auth, wake))
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		return fail(err)
	}
	observed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			fmt.Printf("Push HTTP request: encoding=%q size=%d\n", r.Header.Get("Content-Encoding"), r.ContentLength)
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: observed, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	ctx, cancel := signalContext()
	defer cancel()
	if *bootstrapActive {
		if err = b.Connect(ctx); err != nil {
			return fail(err)
		}
		readyCtx, cancelReady := context.WithTimeout(ctx, 30*time.Second)
		ready := false
		for !ready {
			select {
			case <-readyCtx.Done():
				cancelReady()
				b.Disconnect()
				return fail(errors.New("active bootstrap did not become ready"))
			case ev := <-b.Events():
				switch v := ev.(type) {
				case *gm.EventClientReady:
					ready = true
				case *gm.EventUserAlert:
					ready = v.Alert == gm.AlertBrowserActive
				}
			}
		}
		cancelReady()
		fmt.Println("Temporary active bootstrap ready")
	}
	if err = b.RegisterWebPush(ctx, strings.TrimRight(publicURL.String(), "/")+path, key.PublicKey().Bytes(), auth); err != nil {
		return fail(err)
	}
	if data, err = b.MarshalSession(); err != nil {
		return fail(err)
	}
	if err = ss.Save(*account, data); err != nil {
		return fail(err)
	}
	fmt.Println("Push registration succeeded; idle until a push arrives.")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-b.Events():
				fmt.Printf("Background event: %T\n", ev)
				if settings, ok := ev.(*gm.EventSettings); ok && settings.PushEnabled != nil {
					fmt.Printf("Phone push setting: %t\n", *settings.PushEnabled)
				}
				if msg, ok := ev.(*gm.EventMessage); ok && strings.HasPrefix(msg.Message.Text, "Agent GM push test") {
					fmt.Printf("Test message observed: direction=%s\n", msg.Message.Direction())
				}
			}
		}
	}()
	if *initialFetch || *sendTo != "" || *bootstrapActive {
		requestCtx, cancelRequest := context.WithTimeout(ctx, 45*time.Second)
		request := func(requestCtx context.Context) error {
			convs, requestErr := b.ListConversations(requestCtx, gm.FolderInbox, 100)
			fmt.Printf("Startup conversation request: count=%d success=%t\n", len(convs), requestErr == nil)
			if requestErr == nil && *sendTo != "" {
				resolved, e := b.ResolveConversation(requestCtx, []string{*sendTo}, "")
				if e == nil && resolved.Conversation != nil {
					conv := resolved.Conversation
					result, e := b.SendText(requestCtx, gm.SendTextRequest{ConversationID: conv.SourceID, ParticipantID: conv.DefaultOutgoingID, SIMPayload: conv.SIMPayload, TmpID: uuid.NewString(), Text: "Agent GM push test outbound " + time.Now().UTC().Format(time.RFC3339)})
					fmt.Printf("Outbound test: status=%s success=%t\n", result.Status, e == nil && result.Status == gm.SendStatusSuccess)
				} else {
					fmt.Println("Outbound test: conversation resolution failed")
				}
			}
			return requestErr
		}
		if *bootstrapActive {
			err = request(requestCtx)
			b.Disconnect()
		} else {
			err = b.BackgroundRequest(requestCtx, request)
		}
		cancelRequest()
		if err != nil {
			fmt.Printf("Startup batch failed: %v\n", err)
		}
		if data, err = b.MarshalSession(); err == nil {
			err = ss.Save(*account, data)
		}
		if err != nil {
			return fail(err)
		}
		fmt.Println("Startup background sync finished; idle until push")
	}
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case <-wake:
			fmt.Println("Push triggered background fetch")
			batchCtx, cancelBatch := context.WithTimeout(ctx, 45*time.Second)
			err = b.BackgroundRequest(batchCtx, nil)
			cancelBatch()
			if err != nil {
				fmt.Println("Background fetch did not report clean drain")
			}
			if data, err = b.MarshalSession(); err == nil {
				err = ss.Save(*account, data)
			}
			if err != nil {
				return fail(err)
			}
			fmt.Println("Background fetch finished; idle")
		}
	}
}

type probeSubscription struct {
	Private []byte
	Auth    []byte
	Path    string
}

func probePushHandler(private, auth []byte, wake chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		encoding := r.Header.Get("Content-Encoding")
		if encoding != "aes128gcm" && encoding != "aesgcm" {
			http.Error(w, "unsupported encoding", http.StatusUnsupportedMediaType)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
		if err != nil {
			http.Error(w, "invalid push", 400)
			return
		}
		if encoding == "aesgcm" {
			_, err = gm.DecryptLegacyWebPush(private, auth, data, r.Header.Get("Encryption"), r.Header.Get("Crypto-Key"))
		} else {
			_, err = gm.DecryptWebPush(private, auth, data)
		}
		if err != nil {
			http.Error(w, "invalid push", 400)
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}
}
