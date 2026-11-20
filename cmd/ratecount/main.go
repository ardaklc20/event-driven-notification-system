// Command ratecount reads the worker's JSON log lines from standard input and
// prints, per channel, the number of send starts and the largest number of
// them in any half-open one-second window [t, t + 1 s), with the code the
// tests use (R8, K43):
//
//	docker compose logs --no-log-prefix worker | go run ./cmd/ratecount
//
// A send start is a "provider call started" line; its started_at is the
// reading the start gate took (K42). Other lines are skipped. It exits with 1
// when it finds no start line or cannot parse one.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/ratecheck"
)

// startMarker identifies a start line before it is parsed, so a broken start
// line is an error rather than a skipped line.
var startMarker = []byte(`"msg":"provider call started"`)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(in io.Reader, out, errOut io.Writer) int {
	starts, err := read(in)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "ratecount:", err)
		return 1
	}
	for _, r := range ratecheck.PerChannel(starts) {
		_, _ = fmt.Fprintf(out, "channel=%s starts=%d max_window=%d\n", r.Channel, r.Starts, r.MaxWindow)
	}
	_, _ = fmt.Fprintf(out, "workers=%d\n", len(ratecheck.Workers(starts)))
	return 0
}

func read(in io.Reader) ([]ratecheck.Start, error) {
	var starts []ratecheck.Start
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if !bytes.Contains(line, startMarker) {
			continue
		}
		var rec struct {
			Channel   string `json:"channel"`
			WorkerID  string `json:"worker_id"`
			StartedAt string `json:"started_at"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("line %d: start line is not JSON: %w", n, err)
		}
		at, err := time.Parse(time.RFC3339Nano, rec.StartedAt)
		if err != nil || rec.Channel == "" || rec.WorkerID == "" {
			return nil, fmt.Errorf("line %d: start line without channel, worker_id or a valid started_at", n)
		}
		starts = append(starts, ratecheck.Start{Channel: rec.Channel, WorkerID: rec.WorkerID, At: at})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("no start line found")
	}
	return starts, nil
}
