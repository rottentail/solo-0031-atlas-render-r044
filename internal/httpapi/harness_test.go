package httpapi

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type harness struct {
	t      *testing.T
	store  *persist.Store
	worker *engine.Worker
	server *httptest.Server
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := persist.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	worker := engine.New(store)
	if err := worker.Recover(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	api := API{Service: &service.Service{Store: store}, Worker: worker}
	server := httptest.NewServer(api.Handler())
	h := &harness{t: t, store: store, worker: worker, server: server, cancel: cancel, done: done}
	t.Cleanup(func() {
		server.Close()
		cancel()
		<-done
		store.Close()
	})
	return h
}

func (h *harness) do(method, path string, body []byte, headers map[string]string) (int, []byte) {
	h.t.Helper()
	request, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return response.StatusCode, data
}

func (h *harness) json(method, path string, body any, want int, headers map[string]string) map[string]any {
	h.t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	status, data := h.do(method, path, payload, headers)
	if status != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, data)
	}
	if len(data) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		h.t.Fatalf("%s %s: non-JSON body %q: %v", method, path, data, err)
	}
	return out
}

func (h *harness) waitJob(id string, deadline time.Duration) map[string]any {
	h.t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		job := h.json("GET", "/v1/jobs/"+id, nil, 200, nil)
		switch job["state"] {
		case "succeeded", "failed", "cancelled":
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("job %s did not reach a terminal state within %v", id, deadline)
	return nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}
