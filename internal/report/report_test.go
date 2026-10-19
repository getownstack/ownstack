package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Refusals that a retry cannot change are not retried, and their lines are
// counted as not reported.
func TestPermanentRefusals(t *testing.T) {
	var mu sync.Mutex
	var posts [][]any
	code := http.StatusRequestEntityTooLarge
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Lines []any }
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, body.Lines)
		w.WriteHeader(code)
	}))
	defer srv.Close()
	ctx := context.Background()
	r := New(srv.URL, "tok")
	sent := func() int { mu.Lock(); defer mu.Unlock(); return len(posts) }

	r.Line("a")
	r.Line("b")
	r.flush(ctx, nil)
	if sent() != 1 {
		t.Fatalf("413 was retried: %d posts", sent())
	}

	mu.Lock()
	code = http.StatusOK
	mu.Unlock()
	r.Line("c")
	r.flush(ctx, nil)
	mu.Lock()
	got := posts[1]
	mu.Unlock()
	if len(got) != 2 || got[0] != "c" || got[1] != "[2 lines were not reported to Ownstack]" {
		t.Fatalf("second batch = %v", got)
	}

	mu.Lock()
	code = http.StatusConflict // the run is finished
	mu.Unlock()
	r.Line("d")
	r.flush(ctx, nil)
	r.Line("e")
	r.flush(ctx, map[string]any{"done": true})
	if sent() != 3 {
		t.Fatalf("posted to a finished run: %d posts", sent())
	}
}

// A final batch that is too large must not lose the run's result.
func TestFinishSurvivesRefusedBatch(t *testing.T) {
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if lines, _ := body["lines"].([]any); len(lines) > 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		last = body
	}))
	defer srv.Close()
	r := New(srv.URL, "tok")
	r.Start(context.Background())
	r.Line("a")
	r.Line("b")
	r.Finish(nil)
	mu.Lock()
	defer mu.Unlock()
	lines, _ := last["lines"].([]any)
	if last["done"] != true || last["success"] != true || len(lines) != 1 || lines[0] != "[2 lines were not reported to Ownstack]" {
		t.Errorf("last report = %v", last)
	}
}
