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

func TestSendIncludesRating(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)

	if err := Send(Report{Message: "works", Rating: 4}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["rating"] != float64(4) {
		t.Errorf("rating = %v, want 4", got["rating"])
	}
	// Out of range and zero are both "no rating": the field stays away.
	for _, r := range []int{0, 9, -1} {
		if err := Send(Report{Message: "works", Rating: r}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if _, has := got["rating"]; has {
			t.Errorf("rating %d: field present, want absent", r)
		}
	}
}

func TestSendRejectsEmptyReport(t *testing.T) {
	t.Setenv("KEIBIDROP_FEEDBACK_URL", "http://127.0.0.1:1")
	if err := Send(Report{Message: "   "}); err == nil {
		t.Fatal("want error for no message and no rating")
	}
	if err := Send(Report{Message: "   ", Rating: 7}); err == nil {
		t.Fatal("want error for no message and an out of range rating")
	}
}

func TestSendStarsAlone(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)
	if err := Send(Report{Rating: 5, Surface: "mcp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["rating"] != float64(5) || got["message"] != "" {
		t.Errorf("payload = %v", got)
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
