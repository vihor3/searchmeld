package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

const userRequestLogTimeout = 2 * time.Second

type userRequestAuthType string

const (
	userRequestAuthUnknown   userRequestAuthType = "unknown"
	userRequestAuthAnonymous userRequestAuthType = "anonymous"
	userRequestAuthToken     userRequestAuthType = "api_token"
	userRequestAuthAdminKey  userRequestAuthType = "admin_key"
)

type userRequestLogContextKey struct{}

// userRequestLogState is shared only by the synchronous handlers of one request.
// It retains allowlisted observations, never a credential, body or Token model.
type userRequestLogState struct {
	input model.UserRequestLogInput
}

// captureUserRequest must be the first mounted route middleware, before auth or
// body adapters. Its deferred write preserves abnormal unwinding and accounts for
// handler time before beginning independent, bounded best-effort persistence.
func (h *Handler) captureUserRequest(operation string, format model.CompatFormat) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			state := &userRequestLogState{input: newUserRequestLogInput(r, operation, format, started)}
			recorder := &userRequestResponseWriter{ResponseWriter: w, state: state}
			r = r.WithContext(context.WithValue(r.Context(), userRequestLogContextKey{}, state))
			returned := false
			defer func() {
				input := state.input
				input.LatencyMS = time.Since(started).Milliseconds()
				switch {
				case !returned:
					input.Completion = "interrupted"
				case recorder.writeError:
					input.Completion = "write_error"
				case r.Context().Err() != nil:
					input.Completion = "canceled"
				default:
					input.Completion = "completed"
					if recorder.status == 0 {
						recorder.status = http.StatusOK
					}
				}
				if recorder.status != 0 {
					status := recorder.status
					input.HTTPStatus = &status
				}
				h.persistUserRequestLog(input)
			}()
			next.ServeHTTP(recorder, r)
			returned = true
		})
	}
}

// newUserRequestLogInput snapshots route metadata after server identity and peer
// middleware. Generated IDs remain exact; storage rejects invalid IDs rather than
// clipping a correlation key. Only bounded display fields may be sanitized.
func newUserRequestLogInput(r *http.Request, operation string, format model.CompatFormat, started time.Time) model.UserRequestLogInput {
	input := model.UserRequestLogInput{
		RequestID:    RequestID(r.Context()),
		CreatedAt:    started,
		Operation:    operation,
		CompatFormat: string(format),
		Method:       boundedUserRequestText(r.Method, model.UserRequestLogMethodMaxBytes),
		Path:         boundedUserRequestText(r.URL.Path, model.UserRequestLogPathMaxBytes),
		AuthType:     string(userRequestAuthUnknown),
	}
	if ip := net.ParseIP(clientIP(r)); ip != nil {
		input.ClientIP = ip.String()
	}
	return input
}

// boundedUserRequestText removes invalid UTF-8 and control characters, clipping
// only at a complete rune within the byte ceiling. It is never used for IDs.
func boundedUserRequestText(value string, limit int) string {
	var result strings.Builder
	for _, character := range strings.ToValidUTF8(value, "") {
		if unicode.IsControl(character) {
			continue
		}
		if result.Len()+utf8.RuneLen(character) > limit {
			break
		}
		result.WriteRune(character)
	}
	return result.String()
}

// observeUserRequestIdentity copies verified identity before admission rejection;
// anonymous callers must be explicitly established by an auth bypass or public
// MCP dispatch. Calls outside an instrumented route have no effect.
func observeUserRequestIdentity(ctx context.Context, kind userRequestAuthType, tokenID int64, name string) {
	state, ok := ctx.Value(userRequestLogContextKey{}).(*userRequestLogState)
	if !ok {
		return
	}
	state.input.AuthType = string(kind)
	state.input.APITokenID = nil
	state.input.TokenName = ""
	if kind == userRequestAuthToken {
		state.input.APITokenID = &tokenID
		state.input.TokenName = boundedUserRequestText(name, model.UserRequestLogTokenNameMaxBytes)
	}
}

// observeUserRequestExecution records the exact selected execution key even if
// execution persistence subsequently fails. Call only at an actual execution or
// rejected-Extract write boundary, never during request validation.
func observeUserRequestExecution(ctx context.Context, requestID string) {
	if state, ok := ctx.Value(userRequestLogContextKey{}).(*userRequestLogState); ok {
		state.input.ExecutionRequestID = &requestID
	}
}

// observeUserRequestMCP counts typed dispatch outcomes without inspecting content,
// IDs or error data. Counts saturate at the existing maximum batch size.
func observeUserRequestMCP(ctx context.Context, response mcpResponse) {
	state, ok := ctx.Value(userRequestLogContextKey{}).(*userRequestLogState)
	if !ok {
		return
	}
	if response.Error != nil && state.input.MCPErrorCount < maxMCPBatchRequests {
		state.input.MCPErrorCount++
	}
	if result, ok := response.Result.(map[string]interface{}); ok {
		if failed, ok := result["isError"].(bool); ok && failed && state.input.MCPToolErrorCount < maxMCPBatchRequests {
			state.input.MCPToolErrorCount++
		}
	}
}

// observeMCPTransportError counts a bounded transport/encoding error at its typed
// writer boundary. Unwrapping reaches the request observer without parsing bytes.
func observeMCPTransportError(w http.ResponseWriter) {
	for {
		if recorder, ok := w.(*userRequestResponseWriter); ok {
			if recorder.state != nil && recorder.state.input.MCPErrorCount < maxMCPBatchRequests {
				recorder.state.input.MCPErrorCount++
			}
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

// persistUserRequestLog uses its own finite context and never changes accounting
// or response state. Fixed error categories exclude database detail and payloads.
func (h *Handler) persistUserRequestLog(input model.UserRequestLogInput) {
	if h.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), userRequestLogTimeout)
	defer cancel()
	if err := h.store.RecordUserRequestLog(ctx, input); err != nil {
		category := "storage_error"
		if errors.Is(err, context.DeadlineExceeded) {
			category = "deadline_exceeded"
		} else if errors.Is(err, context.Canceled) {
			category = "canceled"
		}
		h.logError("user_request_log_failed", map[string]interface{}{
			"request_id": input.RequestID,
			"error":      category,
		})
	}
}

type userRequestResponseWriter struct {
	http.ResponseWriter
	state      *userRequestLogState
	status     int
	writeError bool
}

// Unwrap preserves ResponseController access to underlying transport capabilities.
func (w *userRequestResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// WriteHeader forwards every call but observes only the first final status after
// the underlying writer accepts it. Informational responses do not commit status.
func (w *userRequestResponseWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	if w.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.status = status
	}
}

// Write leaves content sniffing, bytes and returned errors to the transport while
// observing its implicit 200 and any partial/failed write.
func (w *userRequestResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if err != nil {
		w.writeError = true
	}
	return n, err
}

// FlushError forwards supported flushes through ResponseController without
// claiming the legacy Flusher interface on writers that cannot flush.
func (w *userRequestResponseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if err != nil {
			w.writeError = true
		}
	}
	return err
}

// userRequestLogs serves only metadata under the mounted admin boundary, using
// configured defaults for invalid limits and capping every read at 1000 rows.
func (h *Handler) userRequestLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		settings, err := h.store.RuntimeSettings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not load request logs")
			return
		}
		limit = settings.SearchLogsLimit
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	logs, err := h.store.ListUserRequestLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load request logs")
		return
	}
	if logs == nil {
		logs = []model.UserRequestLog{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": logs})
}

// userRequestLogDetail distinguishes invalid/missing entries from store failure;
// a missing execution remains an explicit null link alongside the entry snapshot.
func (h *Handler) userRequestLogDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	entry, executionID, err := h.store.GetUserRequestLog(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "request log not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load request log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"log": entry, "execution_log_id": executionID})
}
