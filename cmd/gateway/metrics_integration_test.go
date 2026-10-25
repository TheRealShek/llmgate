package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLlamaMetrics checks authenticated real-backend streaming reaches the public Prometheus scrape.
func TestLlamaMetrics(t *testing.T) {
	address := os.Getenv("LLMGATE_TEST_LLAMA_URL")
	if address == "" {
		t.Skip("set LLMGATE_TEST_LLAMA_URL for GPU metric integration")
	}

	public, secret, id, _, _ := quotaTestGateway(t, address, 0)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/v1/chat/completions", strings.NewReader(`{"model":"glm-edge-4b-chat","messages":[{"role":"user","content":"Count from one to ten."}],"max_tokens":50,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")

	response, err := public.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	_, err = io.Copy(io.Discard, response.Body)

	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal(response.StatusCode, err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for {
		response, err := public.Client().Get(public.URL + "/v1/metrics")
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(response.Body)

		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatal(response.StatusCode, err)
		}

		data := string(body)
		if strings.Contains(data, fmt.Sprintf(`gateway_requests_total{backend="primary",key="%d",status="200"} 1`, id)) &&
			strings.Contains(data, `gateway_ttft_seconds_count{backend="primary"} 1`) &&
			strings.Contains(data, `gateway_request_duration_seconds_count{backend="primary"} 1`) &&
			strings.Contains(data, `gateway_active_streams{backend="primary"} 0`) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("final metrics did not appear", data)
		}

		time.Sleep(10 * time.Millisecond)
	}
}
