package api

import (
	"errors"
	"net/http"
	"time"

	"print-agent/internal/config"
	"print-agent/internal/events"
)

type webSocketSettingsResponse struct {
	AgentID                     string                        `json:"agentId"`
	Mode                        config.WebSocketMode          `json:"mode"`
	ServerPath                  string                        `json:"serverPath"`
	ServerBindAddress           string                        `json:"serverBindAddress"`
	AllowNonLoopback            bool                          `json:"allowNonLoopback"`
	ServerAuthRequired          bool                          `json:"serverAuthRequired"`
	ServerTLS                   bool                          `json:"serverTls"`
	TLSCertPath                 string                        `json:"tlsCertPath,omitempty"`
	TLSKeyRef                   string                        `json:"tlsKeyRef,omitempty"`
	ClientQueueCapacity         int                           `json:"clientQueueCapacity"`
	ConnectionLimit             int                           `json:"connectionLimit"`
	HeartbeatMs                 int64                         `json:"heartbeatMs"`
	WriteTimeoutMs              int64                         `json:"writeTimeoutMs"`
	MaxMessageBytes             int64                         `json:"maxMessageBytes"`
	ReplayLimit                 int                           `json:"replayLimit"`
	EventRetentionSecond        int64                         `json:"eventRetentionSeconds"`
	MaxUnacknowledgedAgeSeconds int64                         `json:"maxUnacknowledgedAgeSeconds"`
	DeadLetterRetentionSeconds  int64                         `json:"deadLetterRetentionSeconds"`
	EventDiskHighWaterBytes     int64                         `json:"eventDiskHighWaterBytes"`
	AllowedOrigins              []string                      `json:"allowedOrigins"`
	Destination                 *webSocketDestinationResponse `json:"destination,omitempty"`
	RestartRequired             bool                          `json:"restartRequired,omitempty"`
}

type webSocketDestinationResponse struct {
	ID                    string   `json:"id"`
	Enabled               bool     `json:"enabled"`
	Endpoint              string   `json:"endpoint"`
	AuthType              string   `json:"authType"`
	SecretRef             string   `json:"secretRef,omitempty"`
	CustomCAPath          string   `json:"customCaPath,omitempty"`
	Categories            []string `json:"categories"`
	ConnectTimeoutMs      int64    `json:"connectTimeoutMs"`
	HeartbeatMs           int64    `json:"heartbeatMs"`
	StaleTimeoutMs        int64    `json:"staleTimeoutMs"`
	WriteTimeoutMs        int64    `json:"writeTimeoutMs"`
	ReconnectMinMs        int64    `json:"reconnectMinMs"`
	ReconnectMaxMs        int64    `json:"reconnectMaxMs"`
	ReconnectJitter       float64  `json:"reconnectJitter"`
	AckTimeoutMs          int64    `json:"ackTimeoutMs"`
	OutboundQueueCapacity int      `json:"outboundQueueCapacity"`
}

func settingsResponse(s config.WebSocketSettings, d *config.WebSocketDestination) webSocketSettingsResponse {
	response := webSocketSettingsResponse{AgentID: s.AgentID, Mode: s.Mode, ServerPath: s.ServerPath,
		ServerBindAddress: s.ServerBindAddress, AllowNonLoopback: s.AllowNonLoopback,
		ServerAuthRequired: s.ServerAuthRequired, ServerTLS: s.ServerTLS, TLSCertPath: s.TLSCertPath,
		TLSKeyRef: s.TLSKeyRef, ClientQueueCapacity: s.ClientQueueCapacity, ConnectionLimit: s.ConnectionLimit,
		HeartbeatMs: s.Heartbeat.Milliseconds(), WriteTimeoutMs: s.WriteTimeout.Milliseconds(),
		MaxMessageBytes: s.MaxMessageBytes, ReplayLimit: s.ReplayLimit,
		EventRetentionSecond:        int64(s.EventRetention.Seconds()),
		MaxUnacknowledgedAgeSeconds: int64(s.MaxUnacknowledgedAge.Seconds()),
		DeadLetterRetentionSeconds:  int64(s.DeadLetterRetention.Seconds()),
		EventDiskHighWaterBytes:     s.EventDiskHighWater,
		AllowedOrigins:              s.AllowedOrigins}
	if d != nil {
		response.Destination = &webSocketDestinationResponse{ID: d.ID, Enabled: d.Enabled,
			Endpoint: d.Endpoint, AuthType: d.AuthType, SecretRef: d.SecretRef, CustomCAPath: d.CustomCAPath,
			Categories: d.Categories, ConnectTimeoutMs: d.ConnectTimeout.Milliseconds(),
			HeartbeatMs: d.Heartbeat.Milliseconds(), StaleTimeoutMs: d.StaleTimeout.Milliseconds(),
			WriteTimeoutMs: d.WriteTimeout.Milliseconds(), ReconnectMinMs: d.ReconnectMin.Milliseconds(),
			ReconnectMaxMs: d.ReconnectMax.Milliseconds(), ReconnectJitter: d.ReconnectJitter,
			AckTimeoutMs: d.AckTimeout.Milliseconds(), OutboundQueueCapacity: d.OutboundQueueCapacity}
	}
	return response
}

func (s *Server) handleGetWebSocketSettings(w http.ResponseWriter, _ *http.Request) {
	settings, err := s.configRepo.WebSocketSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	var destination *config.WebSocketDestination
	if value, err := s.configRepo.EnabledWebSocketDestination(); err == nil {
		destination = &value
	} else if !errors.Is(err, config.ErrNotFound) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse(settings, destination))
}

func (s *Server) handlePutWebSocketSettings(w http.ResponseWriter, r *http.Request) {
	var body webSocketSettingsResponse
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_json", Error: err.Error()})
		return
	}
	current, err := s.configRepo.WebSocketSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	current.Mode, current.ServerPath, current.ServerBindAddress = body.Mode, body.ServerPath, body.ServerBindAddress
	current.AllowNonLoopback, current.ServerAuthRequired, current.ServerTLS = body.AllowNonLoopback, body.ServerAuthRequired, body.ServerTLS
	current.TLSCertPath, current.TLSKeyRef = body.TLSCertPath, body.TLSKeyRef
	current.ClientQueueCapacity, current.ConnectionLimit = body.ClientQueueCapacity, body.ConnectionLimit
	current.Heartbeat, current.WriteTimeout = time.Duration(body.HeartbeatMs)*time.Millisecond, time.Duration(body.WriteTimeoutMs)*time.Millisecond
	current.MaxMessageBytes, current.ReplayLimit = body.MaxMessageBytes, body.ReplayLimit
	current.EventRetention = time.Duration(body.EventRetentionSecond) * time.Second
	current.MaxUnacknowledgedAge = time.Duration(body.MaxUnacknowledgedAgeSeconds) * time.Second
	current.DeadLetterRetention = time.Duration(body.DeadLetterRetentionSeconds) * time.Second
	current.EventDiskHighWater = body.EventDiskHighWaterBytes
	current.AllowedOrigins = body.AllowedOrigins
	if err := current.Validate(current.ServerBindAddress); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_websocket_settings", Error: err.Error()})
		return
	}
	var destination *config.WebSocketDestination
	disableDestinations := false
	if body.Destination != nil {
		d := config.WebSocketDestination{ID: body.Destination.ID, Enabled: body.Destination.Enabled,
			Endpoint: body.Destination.Endpoint, AuthType: body.Destination.AuthType,
			SecretRef: body.Destination.SecretRef, CustomCAPath: body.Destination.CustomCAPath,
			Categories: body.Destination.Categories, ConnectTimeout: time.Duration(body.Destination.ConnectTimeoutMs) * time.Millisecond,
			Heartbeat:             time.Duration(body.Destination.HeartbeatMs) * time.Millisecond,
			StaleTimeout:          time.Duration(body.Destination.StaleTimeoutMs) * time.Millisecond,
			WriteTimeout:          time.Duration(body.Destination.WriteTimeoutMs) * time.Millisecond,
			ReconnectMin:          time.Duration(body.Destination.ReconnectMinMs) * time.Millisecond,
			ReconnectMax:          time.Duration(body.Destination.ReconnectMaxMs) * time.Millisecond,
			ReconnectJitter:       body.Destination.ReconnectJitter,
			AckTimeout:            time.Duration(body.Destination.AckTimeoutMs) * time.Millisecond,
			OutboundQueueCapacity: body.Destination.OutboundQueueCapacity}
		if err := d.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_websocket_destination", Error: err.Error()})
			return
		}
		destination = &d
	} else if current.Mode != config.WebSocketClient && current.Mode != config.WebSocketBoth {
		disableDestinations = true
	} else {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "missing_websocket_destination", Error: "client mode requires a destination"})
		return
	}
	if err := s.configRepo.SaveWebSocketSettings(current); err != nil {
		writeError(w, err)
		return
	}
	if err := s.configRepo.ReplaceWebSocketOrigins(current.AllowedOrigins); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Code: "invalid_websocket_origin", Error: err.Error()})
		return
	}
	if destination != nil {
		if err := s.configRepo.SaveWebSocketDestination(*destination); err != nil {
			writeError(w, err)
			return
		}
	} else if disableDestinations {
		if err := s.configRepo.DisableWebSocketDestinations(); err != nil {
			writeError(w, err)
			return
		}
	}
	response := settingsResponse(current, destination)
	response.RestartRequired = true
	s.bus.Publish(events.Event{Type: events.ConfigChanged,
		Metadata: map[string]any{"setting": "websocket", "restartRequired": true}})
	writeJSON(w, http.StatusOK, response)
}
