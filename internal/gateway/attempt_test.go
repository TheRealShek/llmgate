package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDeliveryCommitBoundary checks headers, writes, and flushes commit immediately without response buffering.
func TestDeliveryCommitBoundary(t *testing.T) {
	for _, operation := range []string{"header map", "informational", "final headers", "body", "flush"} {
		t.Run(operation, func(t *testing.T) {
			underlying := httptest.NewRecorder()
			writer := &deliveryWriter{ResponseWriter: underlying, requestID: "gateway-id"}

			switch operation {
			case "header map":
				writer.Header().Set("Content-Type", "application/json")
			case "informational":
				writer.WriteHeader(http.StatusEarlyHints)
			case "final headers":
				writer.WriteHeader(http.StatusOK)
			case "body":
				_, _ = writer.Write([]byte("first bytes"))
			case "flush":
				if err := http.NewResponseController(writer).Flush(); err != nil {
					t.Fatal(err)
				}
			}

			if writer.committed.Load() != (operation != "header map") {
				t.Fatal("incorrect commitment")
			}

			if writer.final.Load() != (operation != "header map" && operation != "informational") {
				t.Fatal("incorrect final commitment")
			}

			if operation == "body" && underlying.Body.String() != "first bytes" {
				t.Fatal("response was buffered")
			}

			if writer.final.Load() && underlying.Header().Get("X-Request-ID") != "gateway-id" {
				t.Fatal("gateway ID lost at final commitment")
			}
		})
	}
}

type deliveryDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *deliveryDeadlineWriter) SetWriteDeadline(at time.Time) error { w.deadline = at; return nil }

// TestDeliveryResponseControl checks deadlines reach the underlying writer without committing the response.
func TestDeliveryResponseControl(t *testing.T) {
	underlying := &deliveryDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := &deliveryWriter{ResponseWriter: underlying}

	deadline := time.Now().Add(time.Second)
	if err := http.NewResponseController(writer).SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	if underlying.deadline != deadline || writer.committed.Load() {
		t.Fatal("response control changed commitment")
	}
}
