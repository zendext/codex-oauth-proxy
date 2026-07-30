package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminUsageSnapshotTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/usage" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/usage", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("user_id") != "usr_1" {
			t.Fatalf("user_id query = %q, want usr_1", r.URL.Query().Get("user_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":[{"user_id":"usr_1","name":"Alice","api_key_id":"key_1","key_hash":"hash","masked_key":"cop_****abcd","windows":{"5h":{"request_count":1,"total_tokens":20},"7d":{"request_count":2,"total_tokens":50}}}]}`))
	}))
	defer server.Close()

	output := runAdminTestCommand(t, server.URL, "usage", "snapshot", "--user-id", "usr_1")
	if !strings.Contains(output, "TOKENS_5H") || !strings.Contains(output, "50") {
		t.Fatalf("output = %q, want snapshot table", output)
	}
}

func TestAdminUsageTimeseriesQueryAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/usage/timeseries" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/usage/timeseries", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("window") != "7d" || query.Get("step") != "1h" || query.Get("fill") != "zero" {
			t.Fatalf("query = %s, want window=7d step=1h fill=zero", r.URL.RawQuery)
		}
		if got := strings.Join(query["group_by"], ","); got != "user,model" {
			t.Fatalf("group_by = %q, want user,model", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"window":"7d","step":"1h","group_by":["user","model"],"series":[{"bucket_start":"2026-06-29T00:00:00Z","user_id":"usr_1","name":"Alice","model":"gpt-5.3-codex","request_count":1,"total_tokens":20}]}`))
	}))
	defer server.Close()

	output := runAdminTestCommand(
		t,
		server.URL,
		"usage", "timeseries",
		"--window", "7d",
		"--step", "1h",
		"--group-by", "user,model",
		"--fill", "zero",
		"--json",
	)
	if !strings.Contains(output, `"series"`) || !strings.Contains(output, `"gpt-5.3-codex"`) {
		t.Fatalf("json output = %q, want timeseries payload", output)
	}
}
