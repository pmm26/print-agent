package websocket

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	ws "github.com/coder/websocket"

	"print-agent/internal/config"
	"print-agent/internal/events"
)

type Client struct {
	db          *sql.DB
	destination config.WebSocketDestination
	publisher   events.Publisher
	outbox      outbox

	mu               sync.Mutex
	state            string
	nextReconnect    *time.Time
	lastConnected    *time.Time
	lastDelivery     *time.Time
	deliveryFailures uint64
}

func NewClient(db *sql.DB, destination config.WebSocketDestination, publisher events.Publisher) *Client {
	return &Client{db: db, destination: destination, publisher: publisher, outbox: outbox{db: db}, state: "disconnected"}
}

func (c *Client) Run(ctx context.Context) {
	_ = c.outbox.recoverInflight(c.destination.ID)
	delay := c.destination.ReconnectMin
	for ctx.Err() == nil {
		conn, err := c.dial(ctx)
		if err == nil {
			// An acknowledgement may have reached us just before SQLite became
			// unavailable. Requeue every uncommitted inflight delivery on each
			// new connection; the peer may see a duplicate and must deduplicate
			// by eventId/sequence, which is the promised at-least-once contract.
			if recoverErr := c.outbox.recoverInflight(c.destination.ID); recoverErr != nil {
				err = recoverErr
			} else {
				delay = c.destination.ReconnectMin
				c.setConnected()
				c.publishLocal(events.WebSocketOutboundConnected, nil)
				err = c.deliver(ctx, conn)
			}
			conn.CloseNow()
			c.publishLocal(events.WebSocketOutboundDisconnected, map[string]any{"reason": safeError(err)})
		}
		if ctx.Err() != nil {
			return
		}
		c.setDisconnected(err)
		jittered := jitter(delay, c.destination.ReconnectJitter)
		next := time.Now().UTC().Add(jittered)
		c.mu.Lock()
		c.nextReconnect = &next
		c.mu.Unlock()
		c.publishLocal(events.WebSocketReconnectScheduled, map[string]any{"delayMs": jittered.Milliseconds()})
		if !wait(ctx, jittered) {
			return
		}
		delay = min(delay*2, c.destination.ReconnectMax)
	}
}

func (c *Client) dial(parent context.Context) (*ws.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, c.destination.ConnectTimeout)
	defer cancel()
	header := http.Header{}
	if c.destination.AuthType == "bearer" {
		secret, err := resolveSecret(c.destination.SecretRef)
		if err != nil {
			return nil, err
		}
		header.Set("Authorization", "Bearer "+secret)
	}
	httpClient, err := c.httpClient()
	if err != nil {
		return nil, err
	}
	conn, _, err := ws.Dial(ctx, c.destination.Endpoint, &ws.DialOptions{
		HTTPClient: httpClient, HTTPHeader: header, Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		return nil, fmt.Errorf("outbound WebSocket dial failed: %w", err)
	}
	return conn, nil
}

func (c *Client) httpClient() (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.destination.CustomCAPath != "" {
		pem, err := os.ReadFile(c.destination.CustomCAPath)
		if err != nil {
			return nil, fmt.Errorf("read custom CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("custom CA file contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

func (c *Client) deliver(ctx context.Context, conn *ws.Conn) error {
	heartbeat := time.NewTicker(c.destination.Heartbeat)
	defer heartbeat.Stop()
	for ctx.Err() == nil {
		d, err := c.outbox.claim(c.destination.ID, c.destination.AckTimeout)
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-heartbeat.C:
				// Ping waits for its pong. StaleTimeout therefore bounds how long an
				// apparently open but unresponsive connection may survive while idle.
				pingCtx, cancel := context.WithTimeout(ctx, min(c.destination.WriteTimeout, c.destination.StaleTimeout))
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					return err
				}
			case <-time.After(time.Second):
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := writeJSON(ctx, conn, c.destination.WriteTimeout, map[string]any{"type": "event", "event": d.Event}); err != nil {
			c.retryDelivery(d, "write_failed", safeError(err))
			c.deliveryFailed(err)
			return err
		}
		ackCtx, cancel := context.WithTimeout(ctx, c.destination.AckTimeout)
		_, payload, err := conn.Read(ackCtx)
		cancel()
		if err != nil {
			c.retryDelivery(d, "ack_timeout", safeError(err))
			c.deliveryFailed(err)
			return err
		}
		var response struct {
			Type     string `json:"type"`
			EventID  string `json:"eventId"`
			Sequence int64  `json:"sequence"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(payload, &response); err != nil || response.Type != "ack" ||
			response.EventID != d.Event.EventID || response.Sequence != d.Event.Sequence {
			protocolErr := errors.New("remote returned an invalid event acknowledgement")
			c.retryDelivery(d, "invalid_ack", protocolErr.Error())
			c.deliveryFailed(protocolErr)
			return protocolErr
		}
		if err := c.outbox.ack(d); err != nil {
			return err
		}
		c.publishLocal(events.WebSocketEventAcknowledged, map[string]any{
			"eventId": d.Event.EventID, "sequence": d.Event.Sequence,
		})
		now := time.Now().UTC()
		c.mu.Lock()
		c.lastDelivery = &now
		c.mu.Unlock()
	}
	return ctx.Err()
}

func (c *Client) retryDelivery(d delivery, code, message string) {
	deadLettered := d.Attempt >= maxDeliveryAttempts
	if err := c.outbox.retry(d, c.destination.ReconnectMin, code, message); err == nil && deadLettered {
		c.publishLocal(events.WebSocketDeadLettered, map[string]any{
			"eventId": d.Event.EventID, "sequence": d.Event.Sequence, "reason": code,
		})
	}
}

func resolveSecret(ref string) (string, error) {
	var value []byte
	var err error
	switch {
	case strings.HasPrefix(ref, "env:"):
		secret, ok := os.LookupEnv(strings.TrimPrefix(ref, "env:"))
		if !ok {
			return "", errors.New("configured WebSocket credential environment variable is unset")
		}
		value = []byte(secret)
	case strings.HasPrefix(ref, "file:"):
		path := strings.TrimPrefix(ref, "file:")
		if runtime.GOOS != "windows" {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return "", errors.New("configured WebSocket credential could not be read")
			}
			if info.Mode().Perm()&0o077 != 0 {
				return "", errors.New("WebSocket credential file must not be accessible by group or other users")
			}
		}
		value, err = os.ReadFile(path)
	default:
		return "", errors.New("WebSocket credentials must use an env: or file: secret reference")
	}
	if err != nil {
		return "", errors.New("configured WebSocket credential could not be read")
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", errors.New("configured WebSocket credential is empty")
	}
	return secret, nil
}

func (c *Client) setConnected() {
	now := time.Now().UTC()
	c.mu.Lock()
	c.state, c.lastConnected, c.nextReconnect = "connected", &now, nil
	c.mu.Unlock()
	_, _ = c.db.Exec(`UPDATE websocket_destinations SET last_connected_at = ?, updated_at = ? WHERE id = ?`,
		stamp(now), stamp(now), c.destination.ID)
}

func (c *Client) setDisconnected(error) {
	c.mu.Lock()
	c.state = "disconnected"
	c.mu.Unlock()
}

func (c *Client) deliveryFailed(err error) {
	c.mu.Lock()
	c.deliveryFailures++
	c.mu.Unlock()
	c.publishLocal(events.WebSocketDeliveryFailed, map[string]any{"error": safeError(err)})
}

func (c *Client) publishLocal(eventType string, metadata map[string]any) {
	if c.publisher != nil {
		c.publisher.Publish(events.Event{Type: eventType, PublishScope: events.ScopeLocal, Metadata: metadata})
	}
}

func (c *Client) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"state": c.state, "nextReconnectAt": c.nextReconnect,
		"lastConnectedAt": c.lastConnected, "lastDeliveryAt": c.lastDelivery,
		"deliveryFailureCount": c.deliveryFailures}
}

func jitter(delay time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return delay
	}
	factor := 1 - ratio + rand.Float64()*(2*ratio)
	return time.Duration(float64(delay) * factor)
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}
