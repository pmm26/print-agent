package config

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

type WebSocketMode string

const (
	WebSocketDisabled WebSocketMode = "disabled"
	WebSocketServer   WebSocketMode = "server"
	WebSocketClient   WebSocketMode = "client"
	WebSocketBoth     WebSocketMode = "both"
)

type WebSocketSettings struct {
	AgentID              string        `json:"agentId"`
	Mode                 WebSocketMode `json:"mode"`
	ServerPath           string        `json:"serverPath"`
	ServerBindAddress    string        `json:"serverBindAddress"`
	AllowNonLoopback     bool          `json:"allowNonLoopback"`
	ServerAuthRequired   bool          `json:"serverAuthRequired"`
	ServerTLS            bool          `json:"serverTls"`
	TLSCertPath          string        `json:"tlsCertPath,omitempty"`
	TLSKeyRef            string        `json:"tlsKeyRef,omitempty"`
	ClientQueueCapacity  int           `json:"clientQueueCapacity"`
	ConnectionLimit      int           `json:"connectionLimit"`
	Heartbeat            time.Duration `json:"-"`
	WriteTimeout         time.Duration `json:"-"`
	MaxMessageBytes      int64         `json:"maxMessageBytes"`
	ReplayLimit          int           `json:"replayLimit"`
	EventRetention       time.Duration `json:"-"`
	MaxUnacknowledgedAge time.Duration `json:"-"`
	DeadLetterRetention  time.Duration `json:"-"`
	EventDiskHighWater   int64         `json:"eventDiskHighWaterBytes"`
	AllowedOrigins       []string      `json:"allowedOrigins"`
}

type WebSocketDestination struct {
	ID                    string        `json:"id"`
	Enabled               bool          `json:"enabled"`
	Endpoint              string        `json:"endpoint"`
	AuthType              string        `json:"authType"`
	SecretRef             string        `json:"secretRef,omitempty"`
	CustomCAPath          string        `json:"customCaPath,omitempty"`
	Categories            []string      `json:"categories"`
	ConnectTimeout        time.Duration `json:"-"`
	Heartbeat             time.Duration `json:"-"`
	StaleTimeout          time.Duration `json:"-"`
	WriteTimeout          time.Duration `json:"-"`
	ReconnectMin          time.Duration `json:"-"`
	ReconnectMax          time.Duration `json:"-"`
	ReconnectJitter       float64       `json:"reconnectJitter"`
	AckTimeout            time.Duration `json:"-"`
	OutboundQueueCapacity int           `json:"outboundQueueCapacity"`
}

func (r *Repository) WebSocketSettings() (WebSocketSettings, error) {
	var s WebSocketSettings
	var heartbeat, writeTimeout, retention, maxUnacknowledged, deadRetention int64
	err := r.db.QueryRow(`SELECT agent_id, websocket_mode, websocket_server_path,
		websocket_server_bind_address, websocket_allow_non_loopback, websocket_server_auth_required,
		websocket_server_tls, COALESCE(websocket_tls_cert_path,''), COALESCE(websocket_tls_key_ref,''),
		websocket_client_queue_capacity, websocket_connection_limit, websocket_heartbeat_ms,
		websocket_write_timeout_ms, websocket_max_message_bytes, websocket_replay_limit,
		event_retention_seconds, max_unacknowledged_age_seconds, dead_letter_retention_seconds,
		event_disk_high_water_bytes FROM agent_settings WHERE singleton = 1`).Scan(
		&s.AgentID, &s.Mode, &s.ServerPath, &s.ServerBindAddress, &s.AllowNonLoopback,
		&s.ServerAuthRequired, &s.ServerTLS, &s.TLSCertPath, &s.TLSKeyRef,
		&s.ClientQueueCapacity, &s.ConnectionLimit, &heartbeat, &writeTimeout,
		&s.MaxMessageBytes, &s.ReplayLimit, &retention, &maxUnacknowledged, &deadRetention,
		&s.EventDiskHighWater)
	if err != nil {
		return s, err
	}
	s.Heartbeat = time.Duration(heartbeat) * time.Millisecond
	s.WriteTimeout = time.Duration(writeTimeout) * time.Millisecond
	s.EventRetention = time.Duration(retention) * time.Second
	s.MaxUnacknowledgedAge = time.Duration(maxUnacknowledged) * time.Second
	s.DeadLetterRetention = time.Duration(deadRetention) * time.Second
	rows, err := r.db.Query(`SELECT origin FROM websocket_allowed_origins WHERE enabled = 1 ORDER BY origin`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		if err := rows.Scan(&origin); err != nil {
			return s, err
		}
		s.AllowedOrigins = append(s.AllowedOrigins, origin)
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	return s, nil
}

func (s WebSocketSettings) Validate(httpBindAddress string) error {
	switch s.Mode {
	case WebSocketDisabled, WebSocketServer, WebSocketClient, WebSocketBoth:
	default:
		return fmt.Errorf("invalid WebSocket mode %q", s.Mode)
	}
	if !strings.HasPrefix(s.ServerPath, "/api/v2/events/") || s.ServerPath == "/api/v2/events/ticket" {
		return errors.New("WebSocket server path must be a non-reserved path under /api/v2/events/")
	}
	if (s.Mode == WebSocketServer || s.Mode == WebSocketBoth) && s.ServerBindAddress != httpBindAddress {
		return errors.New("WebSocket server shares the HTTP listener; bind addresses must match")
	}
	if ip := net.ParseIP(s.ServerBindAddress); ip == nil {
		return errors.New("WebSocket bind address must be an IP address")
	} else if !ip.IsLoopback() && !s.AllowNonLoopback {
		return errors.New("non-loopback WebSocket binding requires explicit allowNonLoopback")
	}
	if !net.ParseIP(s.ServerBindAddress).IsLoopback() && !s.ServerTLS {
		return errors.New("non-loopback WebSocket binding requires TLS")
	}
	if !net.ParseIP(s.ServerBindAddress).IsLoopback() && !s.ServerAuthRequired {
		return errors.New("non-loopback WebSocket binding requires authentication")
	}
	if s.ServerTLS && (s.TLSCertPath == "" || !strings.HasPrefix(s.TLSKeyRef, "file:")) {
		return errors.New("WebSocket server TLS requires a certificate path and file: private-key reference")
	}
	if s.ClientQueueCapacity < 1 || s.ClientQueueCapacity > 10000 || s.ConnectionLimit < 1 || s.ConnectionLimit > 1000 {
		return errors.New("WebSocket server queue or connection limit is out of range")
	}
	if s.Heartbeat < time.Second || s.WriteTimeout < time.Second || s.ReplayLimit < 1 || s.ReplayLimit > 5000 {
		return errors.New("WebSocket timing or replay settings are out of range")
	}
	if s.MaxMessageBytes < 1024 || s.MaxMessageBytes > 1024*1024 {
		return errors.New("WebSocket maximum message size is out of range")
	}
	if s.EventRetention < time.Hour || s.MaxUnacknowledgedAge < time.Hour ||
		s.DeadLetterRetention < time.Hour || s.EventDiskHighWater < 1024*1024 {
		return errors.New("event retention or disk limit is out of range")
	}
	return nil
}

func (r *Repository) EnabledWebSocketDestination() (WebSocketDestination, error) {
	var d WebSocketDestination
	var categories string
	var connect, heartbeat, stale, write, reconnectMin, reconnectMax, ack int64
	err := r.db.QueryRow(`SELECT id, enabled, endpoint, auth_type, COALESCE(secret_ref,''),
		COALESCE(custom_ca_path,''), categories_json, connect_timeout_ms, heartbeat_interval_ms,
		stale_timeout_ms, write_timeout_ms, reconnect_min_ms, reconnect_max_ms, reconnect_jitter,
		ack_timeout_ms, outbound_queue_capacity FROM websocket_destinations WHERE enabled = 1`).Scan(
		&d.ID, &d.Enabled, &d.Endpoint, &d.AuthType, &d.SecretRef, &d.CustomCAPath,
		&categories, &connect, &heartbeat, &stale, &write, &reconnectMin, &reconnectMax,
		&d.ReconnectJitter, &ack, &d.OutboundQueueCapacity)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal([]byte(categories), &d.Categories); err != nil {
		return d, fmt.Errorf("decode WebSocket categories: %w", err)
	}
	d.ConnectTimeout = time.Duration(connect) * time.Millisecond
	d.Heartbeat = time.Duration(heartbeat) * time.Millisecond
	d.StaleTimeout = time.Duration(stale) * time.Millisecond
	d.WriteTimeout = time.Duration(write) * time.Millisecond
	d.ReconnectMin = time.Duration(reconnectMin) * time.Millisecond
	d.ReconnectMax = time.Duration(reconnectMax) * time.Millisecond
	d.AckTimeout = time.Duration(ack) * time.Millisecond
	return d, nil
}

func (r *Repository) SaveWebSocketSettings(s WebSocketSettings) error {
	_, err := r.db.Exec(`UPDATE agent_settings SET websocket_mode = ?, websocket_server_path = ?,
		websocket_server_bind_address = ?, websocket_allow_non_loopback = ?, websocket_server_auth_required = ?,
		websocket_server_tls = ?, websocket_tls_cert_path = ?, websocket_tls_key_ref = ?,
		websocket_client_queue_capacity = ?, websocket_connection_limit = ?, websocket_heartbeat_ms = ?,
		websocket_write_timeout_ms = ?, websocket_max_message_bytes = ?, websocket_replay_limit = ?,
		event_retention_seconds = ?, max_unacknowledged_age_seconds = ?, dead_letter_retention_seconds = ?,
		event_disk_high_water_bytes = ?, updated_at = ?
		WHERE singleton = 1`, string(s.Mode), s.ServerPath, s.ServerBindAddress, s.AllowNonLoopback,
		s.ServerAuthRequired, s.ServerTLS, nullString(s.TLSCertPath), nullString(s.TLSKeyRef),
		s.ClientQueueCapacity, s.ConnectionLimit, s.Heartbeat.Milliseconds(), s.WriteTimeout.Milliseconds(),
		s.MaxMessageBytes, s.ReplayLimit, int64(s.EventRetention.Seconds()),
		int64(s.MaxUnacknowledgedAge.Seconds()), int64(s.DeadLetterRetention.Seconds()),
		s.EventDiskHighWater, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (r *Repository) ReplaceWebSocketOrigins(origins []string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE websocket_allowed_origins SET enabled = 0`); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid WebSocket browser origin %q", origin)
		}
		if _, err := tx.Exec(`INSERT INTO websocket_allowed_origins (origin, enabled, created_at, updated_at)
			VALUES (?, 1, ?, ?) ON CONFLICT(origin) DO UPDATE SET enabled=1, updated_at=excluded.updated_at`,
			strings.ToLower(parsed.Scheme)+"://"+strings.ToLower(parsed.Host), now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repository) WebSocketOriginAllowed(origin string) bool {
	var count int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM websocket_allowed_origins WHERE origin = ? AND enabled = 1`, origin).Scan(&count)
	return err == nil && count == 1
}

func (r *Repository) SaveWebSocketDestination(d WebSocketDestination) error {
	if err := d.Validate(); err != nil {
		return err
	}
	categories, err := json.Marshal(d.Categories)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if d.Enabled {
		if _, err := tx.Exec(`UPDATE websocket_destinations SET enabled = 0 WHERE id <> ?`, d.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO websocket_destinations
		(id, enabled, endpoint, auth_type, secret_ref, custom_ca_path, categories_json,
		 connect_timeout_ms, heartbeat_interval_ms, stale_timeout_ms, write_timeout_ms,
		 reconnect_min_ms, reconnect_max_ms, reconnect_jitter, ack_timeout_ms,
		 outbound_queue_capacity, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, endpoint=excluded.endpoint,
		auth_type=excluded.auth_type, secret_ref=excluded.secret_ref, custom_ca_path=excluded.custom_ca_path,
		categories_json=excluded.categories_json, connect_timeout_ms=excluded.connect_timeout_ms,
		heartbeat_interval_ms=excluded.heartbeat_interval_ms, stale_timeout_ms=excluded.stale_timeout_ms,
		write_timeout_ms=excluded.write_timeout_ms, reconnect_min_ms=excluded.reconnect_min_ms,
		reconnect_max_ms=excluded.reconnect_max_ms, reconnect_jitter=excluded.reconnect_jitter,
		ack_timeout_ms=excluded.ack_timeout_ms, outbound_queue_capacity=excluded.outbound_queue_capacity,
		updated_at=excluded.updated_at`, d.ID, d.Enabled, d.Endpoint, d.AuthType,
		nullString(d.SecretRef), nullString(d.CustomCAPath), string(categories),
		d.ConnectTimeout.Milliseconds(), d.Heartbeat.Milliseconds(), d.StaleTimeout.Milliseconds(),
		d.WriteTimeout.Milliseconds(), d.ReconnectMin.Milliseconds(), d.ReconnectMax.Milliseconds(),
		d.ReconnectJitter, d.AckTimeout.Milliseconds(), d.OutboundQueueCapacity, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) DisableWebSocketDestinations() error {
	_, err := r.db.Exec(`UPDATE websocket_destinations SET enabled = 0, updated_at = ? WHERE enabled = 1`,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (d WebSocketDestination) Validate() error {
	u, err := url.Parse(d.Endpoint)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("outbound WebSocket endpoint must be an absolute ws:// or wss:// URL without userinfo, query, or fragment")
	}
	if d.AuthType != "none" && d.AuthType != "bearer" {
		return errors.New("unsupported outbound WebSocket authentication type")
	}
	if d.AuthType == "bearer" && d.SecretRef == "" {
		return errors.New("bearer authentication requires a secret reference")
	}
	if d.SecretRef != "" && !strings.HasPrefix(d.SecretRef, "env:") && !strings.HasPrefix(d.SecretRef, "file:") {
		return errors.New("outbound credentials must use an env: or file: secret reference")
	}
	if u.Scheme == "ws" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("unencrypted ws:// is allowed only for loopback destinations")
		}
	}
	allowedCategories := map[string]bool{"agent": true, "printer": true, "job": true, "print_run": true, "config": true}
	if len(d.Categories) == 0 {
		return errors.New("at least one outbound event category is required")
	}
	for _, category := range d.Categories {
		if !allowedCategories[category] {
			return fmt.Errorf("unsupported outbound event category %q", category)
		}
	}
	if d.ConnectTimeout < time.Second || d.ConnectTimeout > 2*time.Minute ||
		d.Heartbeat < time.Second || d.Heartbeat > 5*time.Minute ||
		d.StaleTimeout < 2*time.Second || d.StaleTimeout > 10*time.Minute ||
		d.WriteTimeout < time.Second || d.WriteTimeout > 2*time.Minute ||
		d.ReconnectMin < 100*time.Millisecond || d.ReconnectMin > 10*time.Minute ||
		d.ReconnectMax < time.Second || d.ReconnectMax > time.Hour ||
		d.AckTimeout < time.Second || d.AckTimeout > 10*time.Minute {
		return errors.New("outbound WebSocket timing setting is out of range")
	}
	if d.StaleTimeout < d.Heartbeat {
		return errors.New("stale timeout must be greater than or equal to the heartbeat interval")
	}
	if d.ReconnectMin > d.ReconnectMax {
		return errors.New("reconnect minimum exceeds maximum")
	}
	if d.ReconnectJitter < 0 || d.ReconnectJitter > 1 {
		return errors.New("reconnect jitter must be between zero and one")
	}
	if d.OutboundQueueCapacity < 1 || d.OutboundQueueCapacity > 100000 {
		return errors.New("outbound queue capacity is out of range")
	}
	return nil
}
