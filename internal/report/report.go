// Package report streams ownstackctl's console output to an Ownstack run so
// the web UI can show progress for runs started by the user on their own
// server. It is opt-in (--report), carries only already-redacted console
// lines, and never fails the run: if Ownstack is unreachable the run
// continues and only the terminal shows progress.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const maxBuffered = 5000

type Reporter struct {
	url, token string
	http       *http.Client

	mu      sync.Mutex
	buf     []string
	dropped int
	failed  bool
	stop    chan struct{}
	stopped chan struct{}
}

func New(url, token string) *Reporter {
	return &Reporter{
		url: url, token: token,
		http:    &http.Client{Timeout: 15 * time.Second},
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Line is a console sink.
func (r *Reporter) Line(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= maxBuffered {
		r.dropped++
		return
	}
	r.buf = append(r.buf, line)
}

// Start flushes buffered lines every second until Finish.
func (r *Reporter) Start(ctx context.Context) {
	go func() {
		defer close(r.stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.flush(ctx, nil)
			case <-r.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Finish sends the remaining lines and the run result.
func (r *Reporter) Finish(runErr error) {
	close(r.stop)
	<-r.stopped
	result := map[string]any{"done": true, "success": runErr == nil}
	if runErr != nil {
		result["error"] = runErr.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r.flush(ctx, result)
}

func (r *Reporter) flush(ctx context.Context, result map[string]any) {
	r.mu.Lock()
	lines := r.buf
	r.buf = nil
	dropped := r.dropped
	r.dropped = 0
	r.mu.Unlock()
	if len(lines) == 0 && result == nil {
		return
	}
	if dropped > 0 {
		lines = append(lines, fmt.Sprintf("[%d lines were not reported to Ownstack]", dropped))
	}
	body := map[string]any{"lines": lines}
	for k, v := range result {
		body[k] = v
	}
	data, _ := json.Marshal(body)

	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = r.post(ctx, data); err == nil {
			return
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.failed {
		r.failed = true
		fmt.Fprintf(os.Stderr, "note: could not report progress to Ownstack (%v); the run continues\n", err)
	}
	// Keep the lines for the next attempt rather than losing them.
	if len(r.buf)+len(lines) <= maxBuffered {
		r.buf = append(lines, r.buf...)
	}
}

func (r *Reporter) post(ctx context.Context, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}
