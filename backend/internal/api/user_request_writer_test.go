package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
	"github.com/vihor3/searchmeld/backend/internal/model"
)

// TestUserRequestWriterWireParity compares headers, bytes and status with the
// same handler run without observation, including content sniffing and trailers.
func TestUserRequestWriterWireParity(t *testing.T) {
	for _, test := range []struct {
		name string
		run  http.HandlerFunc
	}{
		{"empty", func(http.ResponseWriter, *http.Request) {}},
		{"implicit body", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "plain response") }},
		{"empty write", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(nil) }},
		{"implicit then header", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("first write"))
			w.WriteHeader(500)
		}},
		{"header only", func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Fixture", "present") }},
		{"explicit first status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("X-Fixture", "one")
			w.Header().Add("X-Fixture", "two")
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("response\x00bytes"))
		}},
		{"trailers", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Trailer", "X-Fixture-Trailer")
			_, _ = w.Write([]byte("trailer body"))
			w.Header().Set("X-Fixture-Trailer", "after body")
		}},
		{"flush", func(w http.ResponseWriter, _ *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024)
			request := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
			plain := httptest.NewRecorder()
			observed := httptest.NewRecorder()
			test.run.ServeHTTP(plain, request)
			f.h.captureUserRequest("search", model.CompatFormatNative)(test.run).ServeHTTP(observed, request)
			if plain.Code != observed.Code || plain.Body.String() != observed.Body.String() || !reflect.DeepEqual(plain.Result().Header, observed.Result().Header) || !reflect.DeepEqual(plain.Result().Trailer, observed.Result().Trailer) || plain.Flushed != observed.Flushed {
				t.Fatal("entry observer changed response bytes, status, headers, trailers or flush")
			}
			if len(f.store.entries) != 1 || f.store.entries[0].HTTPStatus == nil || *f.store.entries[0].HTTPStatus != plain.Code || f.store.entries[0].Completion != "completed" {
				t.Fatal("normal handler completion was not observed accurately")
			}
		})
	}
}

type userRequestWireWriter struct {
	header       http.Header
	status       int
	statusCalls  []int
	writes       int
	writeErr     error
	writePanic   interface{}
	flushErr     error
	flushes      int
	readDeadline time.Time
}

// Header returns the original live header map; observers must not copy or clear it.
func (w *userRequestWireWriter) Header() http.Header { return w.header }

// WriteHeader models net/http final-status commitment, including informational
// responses and invalid-status panic, while retaining every forwarded call.
func (w *userRequestWireWriter) WriteHeader(status int) {
	if status < 100 || status > 999 {
		panic("invalid fixture status")
	}
	w.statusCalls = append(w.statusCalls, status)
	if w.status == 0 && (status >= 200 || status == 101) {
		w.status = status
	}
}

// Write reports a synthetic partial transport failure after implicit status, or
// panics before commitment, permitting exact error/count propagation assertions.
func (w *userRequestWireWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.writePanic != nil {
		panic(w.writePanic)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.writeErr != nil {
		return len(body) / 2, w.writeErr
	}
	return len(body), nil
}

// FlushError commits implicit headers before reporting the configured failure.
func (w *userRequestWireWriter) FlushError() error {
	w.flushes++
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.flushErr
}

// SetReadDeadline records ResponseController forwarding through both wrappers.
func (w *userRequestWireWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return nil
}

// TestUserRequestWriterInformationalAndController checks first-final status under
// the real outer middleware, plus flush/deadline forwarding without a facade.
func TestUserRequestWriterInformationalAndController(t *testing.T) {
	for _, codes := range [][]int{{103, 100, 204, 503}, {103, 101, 204}, {103}} {
		f := newUserRequestTestFixture(t, 1024)
		writer := &userRequestWireWriter{header: make(http.Header)}
		server := NewServer(config.Config{}, f.log)
		deadline := time.Now().Add(time.Minute)
		server.Mount(func(r chi.Router) {
			r.With(f.h.captureUserRequest("search", model.CompatFormatNative)).Post("/observed", func(w http.ResponseWriter, _ *http.Request) {
				for _, code := range codes {
					w.WriteHeader(code)
				}
				controller := http.NewResponseController(w)
				if err := controller.SetReadDeadline(deadline); err != nil {
					t.Fatalf("set read deadline: %v", err)
				}
				if len(codes) == 1 {
					if err := controller.Flush(); err != nil {
						t.Fatalf("flush through outer recorder: %v", err)
					}
				}
			})
		})
		server.Router().ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/observed", nil))
		if !reflect.DeepEqual(writer.statusCalls, codes) || !writer.readDeadline.Equal(deadline) || len(f.store.entries) != 1 || f.store.entries[0].HTTPStatus == nil || *f.store.entries[0].HTTPStatus != writer.status {
			t.Fatal("status observation or underlying controller forwarding changed")
		}
		if len(codes) == 1 && (writer.flushes != 1 || writer.status != 200) {
			t.Fatal("informational-only flush lost its implicit final status")
		}
	}
	plain := &struct{ http.ResponseWriter }{httptest.NewRecorder()}
	observer := &userRequestResponseWriter{ResponseWriter: plain}
	if !errors.Is(http.NewResponseController(observer).Flush(), http.ErrNotSupported) || observer.status != 0 || observer.writeError {
		t.Fatal("unsupported flush fabricated a status or transport failure")
	}
	if _, ok := interface{}(observer).(http.Flusher); ok {
		t.Fatal("observer pretends to support the unconditional legacy Flusher interface")
	}
	f := newUserRequestTestFixture(t, 1024)
	writer := &userRequestWireWriter{header: make(http.Header)}
	f.h.captureUserRequest("search", model.CompatFormatNative)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(103)
	})).ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/search", nil))
	if writer.status != 0 || len(f.store.entries) != 1 || f.store.entries[0].HTTPStatus == nil || *f.store.entries[0].HTTPStatus != 200 {
		t.Fatal("normal return after informational headers lost implicit final 200")
	}
}

// TestUserRequestCompletionPrecedence preserves original panic/error identity and
// committed status, while distinguishing interruption, write failure and cancel.
func TestUserRequestCompletionPrecedence(t *testing.T) {
	writeErr := errors.New("synthetic broken transport")
	panicValue := &struct{ reason string }{"original handler panic"}
	for _, test := range []struct {
		name       string
		panic      bool
		status     int
		completion string
	}{
		{"panic before headers", true, 0, "interrupted"},
		{"panic after headers", true, 202, "interrupted"},
		{"write panic", true, 0, "interrupted"},
		{"write failure", false, 200, "write_error"},
		{"flush failure", false, 200, "write_error"},
		{"cancel without headers", false, 0, "canceled"},
		{"cancel after headers", false, 202, "canceled"},
		{"cancel and write failure", false, 200, "write_error"},
		{"panic after write failure", true, 200, "interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newUserRequestTestFixture(t, 1024)
			f.store.entryErr = errors.New("entry storage failed during unwinding")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &userRequestWireWriter{header: make(http.Header)}
			r := httptest.NewRequest(http.MethodPost, "/v1/search", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+adminTestToken)
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if strings.Contains(test.name, "after headers") {
					w.WriteHeader(202)
				}
				if strings.Contains(test.name, "cancel") {
					cancel()
				}
				if test.name == "write panic" {
					writer.writePanic = panicValue
					_, _ = w.Write([]byte("panic body"))
				}
				if strings.Contains(test.name, "write failure") {
					writer.writeErr = writeErr
					n, err := w.Write([]byte("wire-body"))
					if n != 4 || err != writeErr {
						t.Fatal("write count or original error was replaced")
					}
				}
				if test.name == "flush failure" {
					writer.flushErr = writeErr
					if err := http.NewResponseController(w).Flush(); err != writeErr {
						t.Fatal("flush error identity changed")
					}
				}
				if test.panic {
					panic(panicValue)
				}
			})
			f.store.recordHook = func(ctx context.Context, input model.UserRequestLogInput) {
				deadline, ok := ctx.Deadline()
				if ctx.Err() != nil || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second || ctx.Value(userRequestLogContextKey{}) != nil {
					t.Fatal("entry persistence did not receive a detached, finite context")
				}
			}
			var recovered interface{}
			func() {
				defer func() { recovered = recover() }()
				f.h.captureUserRequest("search", model.CompatFormatNative)(f.auth.requireAPIToken(handler)).ServeHTTP(writer, r)
			}()
			if (recovered != nil) != test.panic || (test.panic && recovered != panicValue) {
				t.Fatal("logging swallowed or replaced the original panic")
			}
			if len(f.store.entries) != 1 || f.store.entries[0].Completion != test.completion || writer.status != test.status {
				t.Fatal("completion precedence or original committed response changed")
			}
			entry := f.store.entries[0]
			if test.status == 0 {
				if entry.HTTPStatus != nil {
					t.Fatal("interrupted/canceled request fabricated an HTTP status")
				}
			} else if entry.HTTPStatus == nil || *entry.HTTPStatus != test.status {
				t.Fatal("committed status was lost on failure")
			}
			if !reflect.DeepEqual(f.store.events, []string{"mark", "entry"}) || f.store.usageMarks != 1 || len(f.log.errors) != 1 {
				t.Fatal("entry failure changed ordinary admission or finalization order")
			}
		})
	}
}

// TestUserRequestPersistenceDeadline proves a stalled context-aware store is
// bounded independently of handler timing and cannot replace the response body.
func TestUserRequestPersistenceDeadline(t *testing.T) {
	f := newUserRequestTestFixture(t, 1024)
	var elapsed time.Duration
	f.store.recordHook = func(ctx context.Context, _ model.UserRequestLogInput) {
		started := time.Now()
		select {
		case <-ctx.Done():
			elapsed = time.Since(started)
		case <-time.After(3 * time.Second):
			t.Fatal("entry persistence exceeded its independent two-second deadline")
		}
	}
	f.store.entryErr = context.DeadlineExceeded
	response := httptest.NewRecorder()
	f.h.captureUserRequest("search", model.CompatFormatNative)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Fixture", "unchanged")
		writeError(w, http.StatusBadRequest, "original handler error")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/search", nil))
	if response.Code != 400 || response.Body.String() != "{\"error\":{\"message\":\"original handler error\",\"status\":400}}\n" || response.Header().Get("X-Fixture") != "unchanged" {
		t.Fatal("timed-out entry write changed the original response")
	}
	if len(f.store.entries) != 1 || time.Duration(f.store.entries[0].LatencyMS)*time.Millisecond >= elapsed || f.store.entries[0].Completion != "completed" {
		t.Fatal("entry latency included its own persistence overhead")
	}
	if len(f.log.errors) != 1 || f.log.errors[0].fields["error"] != "deadline_exceeded" {
		t.Fatal("deadline failure did not produce its fixed safe diagnostic")
	}
}
