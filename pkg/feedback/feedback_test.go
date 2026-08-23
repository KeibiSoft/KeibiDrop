package feedback

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendPostsAllFields(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad json: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)

	err := Send(Report{
		Message: "  the mount hangs  ",
		Contact: "a@b.co",
		Version: "v0.4.2",
		Surface: "cli",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["message"] != "the mount hangs" {
		t.Errorf("message = %q", got["message"])
	}
	if got["contact"] != "a@b.co" || got["version"] != "v0.4.2" || got["surface"] != "cli" {
		t.Errorf("fields = %v", got)
	}
	if got["platform"] == "" {
		t.Errorf("platform missing")
	}
}

func TestSendRejectsEmptyMessage(t *testing.T) {
	t.Setenv("KEIBIDROP_FEEDBACK_URL", "http://127.0.0.1:1")
	if err := Send(Report{Message: "   "}); err == nil {
		t.Fatal("want error for empty message")
	}
}

func TestSendCapsMessage(t *testing.T) {
	var gotLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &m)
		gotLen = len(m["message"])
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)
	if err := Send(Report{Message: strings.Repeat("x", 9000)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotLen != maxMessage {
		t.Errorf("message length = %d, want %d", gotLen, maxMessage)
	}
}

func TestSendSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)
	if err := Send(Report{Message: "hi"}); err == nil {
		t.Fatal("want error for 400")
	}
}
