package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
)

func newTestServer(t *testing.T, quotaBytes int64, quotaVols int) *httptest.Server {
	t.Helper()
	b, err := backend.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(b, quotaBytes, quotaVols).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, path, body string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes(), resp.Header
}

func TestControlPlane(t *testing.T) {
	t.Run("idempotent create replays", func(t *testing.T) {
		ts := newTestServer(t, 1<<40, 100)
		code1, body1, _ := post(t, ts, "/v1/volumes", `{"request_id":"r1","name":"v1","size_bytes":100}`)
		code2, body2, hdr := post(t, ts, "/v1/volumes", `{"request_id":"r1","name":"v1","size_bytes":100}`)
		if code1 != 201 || code2 != 201 || !bytes.Equal(body1, body2) || hdr.Get("Idempotent-Replay") != "true" {
			t.Fatalf("codes %d/%d replay=%q bodies %s vs %s", code1, code2, hdr.Get("Idempotent-Replay"), body1, body2)
		}
		// Different request id, same name+size also collapses at the backend.
		code3, _, _ := post(t, ts, "/v1/volumes", `{"request_id":"r2","name":"v1","size_bytes":999}`)
		if code3 != 409 {
			t.Fatalf("conflicting re-create: want 409, got %d", code3)
		}
	})

	t.Run("quota enforced", func(t *testing.T) {
		ts := newTestServer(t, 150, 100)
		if code, _, _ := post(t, ts, "/v1/volumes", `{"name":"a","size_bytes":100}`); code != 201 {
			t.Fatalf("first: %d", code)
		}
		if code, body, _ := post(t, ts, "/v1/volumes", `{"name":"b","size_bytes":100}`); code != 429 {
			t.Fatalf("over quota: want 429, got %d %s", code, body)
		}
	})

	t.Run("snapshot schedule runs via job queue", func(t *testing.T) {
		ts := newTestServer(t, 1<<40, 100)
		if code, body, _ := post(t, ts, "/v1/volumes", `{"name":"data","size_bytes":100}`); code != 201 {
			t.Fatalf("create: %d %s", code, body)
		}
		if code, body, _ := post(t, ts, "/v1/schedules", `{"volume_id":"data","interval_seconds":1,"name_prefix":"auto"}`); code != 201 {
			t.Fatalf("schedule: %d %s", code, body)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(ts.URL + "/v1/snapshots")
			if err != nil {
				t.Fatal(err)
			}
			var snaps []json.RawMessage
			_ = json.NewDecoder(resp.Body).Decode(&snaps)
			resp.Body.Close()
			if len(snaps) >= 1 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("scheduled snapshot never appeared")
	})

	t.Run("probes", func(t *testing.T) {
		ts := newTestServer(t, 1<<40, 100)
		for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
			resp, err := http.Get(ts.URL + p)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%s: %v %v", p, resp.StatusCode, err)
			}
			resp.Body.Close()
		}
	})
}
