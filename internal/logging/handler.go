package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

const redacted = "[REDACTED]"

var bearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)

// Handler sends the same sanitized record to the configured JSON sink and
// the durable system-log store. Store failures are deliberately dropped here:
// logging a log-store failure through this handler would recurse indefinitely.
type Handler struct {
	sink   slog.Handler
	store  *Store
	attrs  []boundAttr
	groups []string
}

type boundAttr struct {
	attr   slog.Attr
	groups []string
}

func NewHandler(sink slog.Handler, store *Store) *Handler {
	return &Handler{sink: sink, store: store}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.sink.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	attrs := make(map[string]any, len(h.attrs)+record.NumAttrs())
	for _, bound := range h.attrs {
		addAttr(attrs, bound.groups, bound.attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		addAttr(attrs, h.groups, attr)
		return true
	})

	created := record.Time
	if created.IsZero() {
		created = time.Now().UTC()
	}
	safeRecord := slog.NewRecord(created, record.Level, sanitizeText(record.Message), record.PC)
	for key, value := range attrs {
		safeRecord.AddAttrs(slog.Any(key, value))
	}
	sinkErr := h.sink.Handle(ctx, safeRecord)
	if h.store != nil {
		_ = h.store.Insert(Record{
			CreatedAt:  created,
			Level:      levelName(record.Level),
			Message:    safeRecord.Message,
			PrinterID:  attributeString(attrs, "printer", "printerId"),
			RunUID:     attributeString(attrs, "printRun", "runUid"),
			Attributes: attrs,
		})
	}
	return sinkErr
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]boundAttr(nil), h.attrs...)
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, boundAttr{attr: attr, groups: append([]string(nil), h.groups...)})
	}
	return &clone
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func addAttr(out map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := strings.Join(append(append([]string(nil), groups...), attr.Key), ".")
	addSanitizedValue(out, key, attr.Value)
}

func addSanitizedValue(out map[string]any, key string, value slog.Value) {
	if value.Kind() == slog.KindGroup {
		for _, child := range value.Group() {
			childKey := child.Key
			if key != "" {
				childKey = key + "." + childKey
			}
			child.Value = child.Value.Resolve()
			addSanitizedValue(out, childKey, child.Value)
		}
		return
	}
	if sensitiveKey(key) {
		out[key] = redacted
		return
	}
	switch value.Kind() {
	case slog.KindString:
		out[key] = sanitizeText(value.String())
	case slog.KindBool:
		out[key] = value.Bool()
	case slog.KindInt64:
		out[key] = value.Int64()
	case slog.KindUint64:
		out[key] = value.Uint64()
	case slog.KindFloat64:
		out[key] = value.Float64()
	case slog.KindDuration:
		out[key] = value.Duration().String()
	case slog.KindTime:
		out[key] = value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			out[key] = sanitizeText(err.Error())
		} else {
			out[key] = sanitizeAny(key, value.Any())
		}
	default:
		out[key] = sanitizeText(value.String())
	}
}

func sanitizeAny(key string, value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[UNAVAILABLE " + sanitizeText(fmt.Sprintf("%T", value)) + "]"
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		return "[UNAVAILABLE]"
	}
	return sanitizeDecoded(key, decoded)
}

func sanitizeDecoded(key string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, child := range typed {
			qualified := childKey
			if key != "" {
				qualified = key + "." + childKey
			}
			if sensitiveKey(qualified) {
				result[childKey] = redacted
			} else {
				result[childKey] = sanitizeDecoded(qualified, child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = sanitizeDecoded(key, child)
		}
		return result
	case string:
		return sanitizeText(typed)
	default:
		return value
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
	for _, fragment := range []string{"authorization", "accesstoken", "bearertoken", "clienttoken",
		"pairingcode", "password", "secret", "cookie", "payload", "receiptdata", "datajson", "requestbody"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return normalized == "token" || normalized == "body" || normalized == "data"
}

func sanitizeText(value string) string {
	return bearerPattern.ReplaceAllString(value, "Bearer "+redacted)
}

func attributeString(attrs map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := attrs[key].(string); ok && value != redacted {
			return value
		}
	}
	return ""
}

func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}
