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
	"errors"
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
	// closed is set once Ownstack refuses the run for good (bad token,
	// unknown or finished run); later lines are dropped without posting.
	closed  bool
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
	if r.closed {
		r.dropped += len(r.buf)
		r.buf = nil
		r.mu.Unlock()
		return
	}
	lines := r.buf
	r.buf = nil
	dropped := r.dropped
	r.dropped = 0
	r.mu.Unlock()
	if len(lines) == 0 && result == nil {
		return
	}
	batch := lines
	if dropped > 0 {
		batch = append(lines[:len(lines):len(lines)], fmt.Sprintf("[%d lines were not reported to Ownstack]", dropped))
	}
	body := map[string]any{"lines": batch}
	for k, v := range result {
		body[k] = v
	}
	data, _ := json.Marshal(body)

	var err error
	var refused statusError
	for attempt := 0; attempt < 3; attempt++ {
		if err = r.post(ctx, data); err == nil {
			return
		}
		if errors.As(err, &refused) && refused.permanent() {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if result != nil && errors.As(err, &refused) && refused.permanent() && !refused.closesRun() {
		// The batch was refused (too large), not the run: send the result
		// alone so the run still finishes in Ownstack.
		alone := map[string]any{"lines": []string{fmt.Sprintf("[%d lines were not reported to Ownstack]", dropped+len(lines))}}
		for k, v := range result {
			alone[k] = v
		}
		data, _ := json.Marshal(alone)
		if err = r.post(ctx, data); err == nil {
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.failed {
		r.failed = true
		fmt.Fprintf(os.Stderr, "note: could not report progress to Ownstack (%v); the run continues\n", err)
	}
	// The count, not the marker line, carries over: the next batch reports it.
	r.dropped += dropped
	switch {
	case errors.As(err, &refused) && refused.permanent():
		// Resending cannot succeed.
		r.dropped += len(lines)
		r.closed = refused.closesRun()
	case len(r.buf)+len(lines) <= maxBuffered:
		// Keep the lines for the next attempt rather than losing them.
		r.buf = append(lines, r.buf...)
	default:
		r.dropped += len(lines)
	}
}

// statusError is an HTTP answer other than success.
type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// permanent answers do not change on retry: the token or run is not accepted
// (401, 404, 409), or this batch is refused (413, 422).
func (e statusError) permanent() bool {
	switch e.code {
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// closesRun answers refuse every later batch too.
func (e statusError) closesRun() bool {
	return e.code == http.StatusUnauthorized || e.code == http.StatusNotFound || e.code == http.StatusConflict
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
		return statusError{res.StatusCode}
	}
	return nil
}
