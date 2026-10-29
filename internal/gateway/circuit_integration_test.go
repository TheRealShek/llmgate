package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func serveCircuitChat(t *testing.T, routes http.Handler, stream bool, writer http.ResponseWriter) {
	t.Helper()

	request := admissionRequest(t.Context(), stream)
	if stream {
		request.Header.Set("X-Test-Mode", "stream")
	}

	defer func() {
		if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
			panic(recovered)
		}
	}()

	routes.ServeHTTP(writer, request)
}

// TestCircuitChatFailover checks real proxy failures accumulate across requests, bypass A, and recover after a trial.
func TestCircuitChatFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				routes, state := failoverGateway(t, "503", 2, 2)

				for range 7 {
					w := httptest.NewRecorder()
					serveCircuitChat(t, routes, stream, w)

					if w.Code != http.StatusOK {
						t.Fatal("failover response", w.Code)
					}
				}

				want := []string{"a", "b", "a", "b", "a", "b", "b", "b", "b", "b"}
				if len(state.generation) != len(want) {
					t.Fatal("unexpected attempts", state.generation)
				}

				for i := range want {
					if state.generation[i] != want[i] {
						t.Fatal("circuit did not bypass A", state.generation)
					}
				}

				if len(state.admissions) != len(want) || len(state.finals) != len(want) || len(state.charges) != len(want) {
					t.Fatal("per-attempt accounting changed", len(state.admissions), len(state.finals), len(state.charges))
				}

				time.Sleep(10 * time.Second)
				state.mu.Lock()
				state.mode = "success"
				state.mu.Unlock()

				for range 4 {
					w := httptest.NewRecorder()
					serveCircuitChat(t, routes, stream, w)

					if w.Code != http.StatusOK {
						t.Fatal("recovery response", w.Code)
					}
				}

				got := state.generation[len(want):]
				if len(got) != 4 || got[0] != "a" || got[1] != "b" || got[2] != "a" || got[3] != "b" {
					t.Fatal("trial failed to restore routing", got)
				}

				for name, q := range state.queues {
					if q.State().Active != 0 || q.State().Waiting != 0 {
						t.Fatal("capacity leaked", name, q.State())
					}
				}
			})
		})
	}
}

// TestCircuitInferenceFailures checks final HTTP errors, transport failures, and interrupted bodies open circuits without retry.
func TestCircuitInferenceFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"500", "503", "transport", "timeout", "partial", "headers", "eof", "backend error"} {
			if !stream && (mode == "eof" || mode == "backend error") {
				continue
			}

			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				routes, state := failoverGateway(t, mode, 1, 1)

				server := httptest.NewServer(routes)
				defer server.Close()

				client := authenticatedClient(server)

				for number := 1; number <= 4; number++ {
					request := admissionRequest(t.Context(), stream)
					request.URL.Scheme = "http"
					request.URL.Host = strings.TrimPrefix(server.URL, "http://")

					request.RequestURI = ""
					if stream {
						request.Header.Set("X-Test-Mode", "stream")
					}

					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}

					body, _ := io.ReadAll(response.Body)

					_ = response.Body.Close()
					if number == 4 && (response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"no_backend"`)) {
						t.Fatal("open circuit did not reject before generation", response.StatusCode, string(body))
					}
				}

				state.mu.Lock()
				defer state.mu.Unlock()

				if len(state.generation) != 3 {
					t.Fatalf("generation attempts = %v; want circuit open after three", state.generation)
				}
			})
		}
	}
}

type failedCircuitClient struct{ *httptest.ResponseRecorder }

func (w *failedCircuitClient) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestCircuitNeutralFailures checks client cancellation, failed delivery, 4xx responses, and durable admission failures do not open a circuit.
func TestCircuitNeutralFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"cancel", "client write", "400", "admission unavailable"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				upstreamMode := mode
				if mode == "client write" {
					upstreamMode = "success"
				}

				routes, state := failoverGateway(t, upstreamMode, 1, 1)
				for range 5 {
					ctx, cancel := context.WithCancel(t.Context())
					state.cancel = cancel

					request := admissionRequest(ctx, stream)
					if stream {
						request.Header.Set("X-Test-Mode", "stream")
					}

					var writer http.ResponseWriter = httptest.NewRecorder()
					if mode == "client write" {
						writer = &failedCircuitClient{httptest.NewRecorder()}
					}

					func() {
						defer func() {
							if p := recover(); p != nil && p != http.ErrAbortHandler {
								panic(p)
							}
						}()

						routes.ServeHTTP(writer, request)
					}()
					cancel()
				}

				if mode == "admission unavailable" {
					if len(state.counts) != 5 || len(state.generation) != 0 {
						t.Fatal("local failure changed routing", state.counts, state.generation)
					}
				} else if len(state.generation) != 5 {
					t.Fatal("neutral failure opened circuit", state.generation)
				}
			})
		}
	}
}
