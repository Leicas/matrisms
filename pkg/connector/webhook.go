package connector

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Leicas/matrisms/pkg/common"
)

// WebhookServer receives VoIP.ms "SMS URL Callback" GETs and nudges the
// owning account's poller for an immediate sweep. We deliberately do NOT
// inject the callback payload straight into the room: the poller is the
// single source of truth (it also catches MMS media reliably via
// getMediaMMS), the callback is just a zero-latency doorbell. Delivery
// therefore stays correct even if VoIP.ms changes callback fields.
//
// Callback URL format to configure per DID in the VoIP.ms portal:
//
//	https://host:29332/sms?token=<secret>&to={TO}&from={FROM}&id={ID}
//
// The listener replies with the literal body "ok" so the optional
// url_callback_retry mode is satisfied.
type WebhookServer struct {
	connector *SMSConnector
	server    *http.Server
}

func NewWebhookServer(sc *SMSConnector) *WebhookServer {
	return &WebhookServer{connector: sc}
}

func (ws *WebhookServer) Start() error {
	cfg := ws.connector.Config.Webhook
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.EffectivePath(), ws.handleCallback)
	ws.server = &http.Server{
		Addr:              cfg.EffectiveListenAddress(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		err := ws.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			ws.connector.Bridge.Log.Error().Err(err).Msg("SMS webhook listener died")
		}
	}()
	ws.connector.Bridge.Log.Info().
		Str("address", cfg.EffectiveListenAddress()).
		Str("path", cfg.EffectivePath()).
		Msg("SMS webhook listener started")
	return nil
}

func (ws *WebhookServer) Stop() {
	if ws.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ws.server.Shutdown(ctx)
		ws.server = nil
	}
}

func (ws *WebhookServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if secret := ws.connector.Config.Webhook.Secret; secret != "" && q.Get("token") != secret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	to := common.NormalizePhone(q.Get("to"))
	ws.connector.Bridge.Log.Debug().
		Str("to", to).
		Str("id", q.Get("id")).
		Msg("SMS webhook callback received")

	// Find every logged-in client that monitors this DID and trigger an
	// immediate poll. Empty `to` (misconfigured callback URL) nudges all.
	triggered := 0
	for _, client := range ws.connector.activeClients() {
		if client.Poller == nil {
			continue
		}
		match := to == ""
		for _, did := range client.DIDs {
			if did == to {
				match = true
				break
			}
		}
		if match {
			client.Poller.TriggerPoll()
			triggered++
		}
	}
	if triggered == 0 {
		ws.connector.Bridge.Log.Warn().Str("to", to).Msg("SMS webhook callback for a DID no login monitors")
	}

	// VoIP.ms url_callback_retry expects the literal body "ok".
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
