// Package feedback posts a user-written support message to the KeibiDrop
// feedback endpoint. Sent on purpose and nothing else: the message, the
// optional reply contact, the app version, the platform, which surface sent
// it and, when the person asks for it, the app's log with names, codes and
// addresses removed (common.SanitizedLogTail).
package feedback

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const DefaultEndpoint = "https://keibidrop.com/feedback"

const maxMessage = 4000

// MaxLogs caps the log a report carries: the newest 4 MiB of sanitized
// text. The endpoint takes no more.
const MaxLogs = 4 << 20

// maxLogsWire keeps the request under the endpoint's 4 MiB: a log that does
// not compress below it is cut to its newer half until it does.
const maxLogsWire = 3 << 20

// ErrSentWithoutLogs: the endpoint refused the report with its log as too
// large, then took it without.
var ErrSentWithoutLogs = errors.New("sent without the log: the endpoint refused its size")

type Report struct {
	Message string `json:"message"`
	Contact string `json:"contact,omitempty"`
	Version string `json:"version,omitempty"`
	Surface string `json:"surface,omitempty"`
	// Rating is 1 to 5 stars, 0 when the person gave none.
	Rating int `json:"rating,omitempty"`
	// Logs is sanitized log text, sent gzipped; empty sends none.
	Logs string `json:"-"`
}

type payload struct {
	Report
	Platform string `json:"platform,omitempty"`
	// LogsGz is the newest MaxLogs bytes of Logs, gzipped, base64.
	LogsGz string `json:"logs_gz,omitempty"`
}

// Send posts the report. Blocks up to 10s, 60s with a log.
// KEIBIDROP_FEEDBACK_URL overrides the endpoint for tests. A rating alone is
// a report; a message alone is a report; both is one too.
func Send(r Report) error {
	r.Message = strings.TrimSpace(r.Message)
	if r.Rating < 1 || r.Rating > 5 {
		r.Rating = 0
	}
	if r.Message == "" && r.Rating == 0 {
		return fmt.Errorf("nothing to send: give a rating from 1 to 5, a message, or both")
	}
	if len(r.Message) > maxMessage {
		r.Message = r.Message[:maxMessage]
	}
	p := payload{Report: r, Platform: runtime.GOOS}
	if r.Logs != "" {
		gz, err := packLogs(r.Logs)
		if err != nil {
			return err
		}
		p.LogsGz = gz
	}
	resp, err := post(p)
	if err != nil {
		return err
	}
	// An endpoint from before logs refuses the larger body: the words go
	// alone rather than not at all.
	if resp.StatusCode == http.StatusRequestEntityTooLarge && p.LogsGz != "" {
		p.LogsGz = ""
		if resp, err = post(p); err != nil {
			return err
		}
		if accepted(resp) {
			return ErrSentWithoutLogs
		}
	}
	if !accepted(resp) {
		return fmt.Errorf("feedback endpoint returned %s", resp.Status)
	}
	return nil
}

func accepted(resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode <= 299
}

func post(p payload) (*http.Response, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	url := os.Getenv("KEIBIDROP_FEEDBACK_URL")
	if url == "" {
		url = DefaultEndpoint
	}
	timeout := 10 * time.Second
	if p.LogsGz != "" {
		// Up to 3 MiB over a slow uplink.
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) // #nosec G704 -- fixed endpoint; the env override exists for tests
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}

// PackLogs is packLogs for a caller with no log file: the browser peer sends
// its in-memory log the way Send sends the desktop's file.
func PackLogs(text string) (string, error) {
	return packLogs(text)
}

// packLogs keeps the newest MaxLogs bytes of text from a line start, gzips
// and base64-encodes them, and halves what it keeps until it fits
// maxLogsWire.
func packLogs(text string) (string, error) {
	for keep := MaxLogs; ; keep /= 2 {
		gz, err := gzipBase64(newestLines(text, keep))
		if err != nil || len(gz) <= maxLogsWire || keep < 1<<10 {
			return gz, err
		}
	}
}

func gzipBase64(s string) (string, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := zw.Write([]byte(s)); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// newestLines returns the last n bytes of s, from the first line start in
// them.
func newestLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
