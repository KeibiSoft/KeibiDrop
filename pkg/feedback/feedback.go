// Package feedback posts a user-written support message to the KeibiDrop
// feedback endpoint. Sent on purpose and nothing else: the message, the
// optional reply contact, the app version, the platform, and which
// surface sent it.
package feedback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const DefaultEndpoint = "https://keibidrop.com/feedback"

const maxMessage = 4000

type Report struct {
	Message string `json:"message"`
	Contact string `json:"contact,omitempty"`
	Version string `json:"version,omitempty"`
	Surface string `json:"surface,omitempty"`
}

type payload struct {
	Report
	Platform string `json:"platform,omitempty"`
}

// Send posts the report. Blocks up to 10s. KEIBIDROP_FEEDBACK_URL
// overrides the endpoint for tests.
func Send(r Report) error {
	r.Message = strings.TrimSpace(r.Message)
	if r.Message == "" {
		return fmt.Errorf("empty message")
	}
	if len(r.Message) > maxMessage {
		r.Message = r.Message[:maxMessage]
	}
	body, err := json.Marshal(payload{Report: r, Platform: runtime.GOOS})
	if err != nil {
		return err
	}
	url := os.Getenv("KEIBIDROP_FEEDBACK_URL")
	if url == "" {
		url = DefaultEndpoint
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) // #nosec G704 -- fixed endpoint; the env override exists for tests
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("feedback endpoint returned %s", resp.Status)
	}
	return nil
}
