package feedback

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// logsOf decodes the logs_gz a request carried, "" when it had none.
func logsOf(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	enc, _ := m["logs_gz"].(string)
	if enc == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("logs_gz is not base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("logs_gz is not gzip: %v", err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(text)
}

func TestSendCarriesTheLogGzipped(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = logsOf(t, body)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)

	log := "time=1 level=INFO msg=one\ntime=2 level=WARN msg=two\n"
	if err := Send(Report{Message: "it hangs", Logs: log}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != log {
		t.Errorf("log = %q, want %q", got, log)
	}
}

func TestSendKeepsTheNewestLogFromALineStart(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = logsOf(t, body)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)

	var b strings.Builder
	for i := 0; b.Len() < MaxLogs+(1<<20); i++ {
		fmt.Fprintf(&b, "time=%d level=DEBUG msg=\"line %d\"\n", i, i)
	}
	log := b.String()
	if err := Send(Report{Message: "big", Logs: log}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got) > MaxLogs || len(got) < MaxLogs-200 {
		t.Errorf("log length = %d, want just under %d", len(got), MaxLogs)
	}
	if !strings.HasPrefix(got, "time=") || !strings.HasSuffix(log, got) {
		t.Errorf("log is not the newest lines from a line start: starts %q", got[:40])
	}
}

// An endpoint from before logs (16 KiB body cap) answers 413: the report
// goes again without the log.
func TestSendRetriesWithoutTheLogWhenRefusedAsTooLarge(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		_, withLog := m["logs_gz"]
		calls = append(calls, fmt.Sprintf("%v:%v", m["message"], withLog))
		if withLog {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)

	err := Send(Report{Message: "words", Logs: "time=1 msg=x\n"})
	if !errors.Is(err, ErrSentWithoutLogs) {
		t.Fatalf("err = %v, want ErrSentWithoutLogs", err)
	}
	if want := []string{"words:true", "words:false"}; strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestSendWithoutLogsSendsNoLogField(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw = string(body)
	}))
	defer srv.Close()
	t.Setenv("KEIBIDROP_FEEDBACK_URL", srv.URL)
	if err := Send(Report{Message: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Contains(raw, "logs") {
		t.Errorf("body names logs without any: %s", raw)
	}
}
