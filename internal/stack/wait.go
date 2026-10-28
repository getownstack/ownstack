package stack

import (
	"context"
	"fmt"
	"time"
)

// waitInterval is how often WaitFor polls.
var waitInterval = 2 * time.Second

// WaitFor polls cond every 2 seconds until it holds, timeout passes, or ctx
// is done. cond returns whether it holds and, when not, why (detail). WaitFor
// logs detail only when it changes, so a long wait prints one line per state
// rather than one per poll. It is for waiting on a condition; Retry is for
// repeating an action that failed.
func WaitFor(ctx context.Context, rt *Runtime, what string, timeout time.Duration, cond func(context.Context) (bool, string)) error {
	deadline := time.Now().Add(timeout)
	logged := false
	last := ""
	for {
		ok, detail := cond(ctx)
		if ok {
			return nil
		}
		if !logged || detail != last {
			if detail == "" {
				rt.Logf("  waiting: %s", what)
			} else {
				rt.Logf("  waiting: %s (%s)", what, detail)
			}
			logged, last = true, detail
		}
		if !time.Now().Before(deadline) {
			if detail == "" {
				return fmt.Errorf("%s: not ready after %s", what, timeout)
			}
			return fmt.Errorf("%s: not ready after %s (%s)", what, timeout, detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitInterval):
		}
	}
}
