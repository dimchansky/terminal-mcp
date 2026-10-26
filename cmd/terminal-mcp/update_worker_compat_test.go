//go:build linux || darwin

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWaitForIdleFenceUpgradesOlderServer(t *testing.T) {
	var began, checked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/maintenance" || r.Method != http.MethodPost {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var request struct {
			ServerID string `json:"server_id"`
			Action   string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.ServerID != "old-server-id" {
			http.Error(w, "wrong server", http.StatusBadRequest)
			return
		}
		switch request.Action {
		case "begin":
			began.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		case "ready":
			checked.Add(1)
			http.Error(w, "unknown action", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := &updateClient{
		listen: strings.TrimPrefix(server.URL, "http://"),
		token:  "test-token", http: server.Client(),
	}
	if err := waitForIdleFence(client, "old-server-id"); err != nil {
		t.Fatalf("legacy idle fence was not recognized: %v", err)
	}
	if began.Load() != 1 || checked.Load() != 1 {
		t.Fatalf("legacy maintenance calls: begin=%d ready=%d", began.Load(), checked.Load())
	}
}
