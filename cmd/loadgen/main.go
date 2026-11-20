// Command loadgen sends the R8 load through the API: -per-channel creates on
// each of the three channels, -concurrency at a time. It prints how many
// creates returned 202 with an ID and exits with 1 if any did not (K43):
//
//	go run ./cmd/loadgen -url http://127.0.0.1:8080 -per-channel 1000
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// recipients are valid recipients for each channel (K11).
var recipients = map[string]string{
	"sms":   "+905551234567",
	"email": "load@example.com",
	"push":  "load-device-token",
}

var channels = []string{"sms", "email", "push"}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("url", "", "API base URL, for example http://127.0.0.1:8080")
	perChannel := fs.Int("per-channel", 1000, "creates per channel")
	concurrency := fs.Int("concurrency", 50, "creates in flight at once")
	timeout := fs.Duration("timeout", 10*time.Second, "timeout of one create")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *baseURL == "" || *perChannel < 1 || *concurrency < 1 {
		_, _ = fmt.Fprintln(errOut, "loadgen: -url is required; -per-channel and -concurrency must be at least 1")
		return 2
	}

	type job struct {
		channel string
		n       int
	}
	jobs := make(chan job)
	var (
		accepted atomic.Int64
		failed   atomic.Int64
		firstErr sync.Once
		wg       sync.WaitGroup
	)
	client := &http.Client{Timeout: *timeout}
	start := time.Now()
	for range *concurrency {
		wg.Go(func() {
			for j := range jobs {
				if err := create(client, *baseURL, j.channel, j.n); err != nil {
					failed.Add(1)
					firstErr.Do(func() { _, _ = fmt.Fprintln(errOut, "loadgen: first failure:", err) })
					continue
				}
				accepted.Add(1)
			}
		})
	}
	// Interleave the channels, so each channel's work arrives over the whole
	// run.
	for n := 1; n <= *perChannel; n++ {
		for _, ch := range channels {
			jobs <- job{channel: ch, n: n}
		}
	}
	close(jobs)
	wg.Wait()

	total := *perChannel * len(channels)
	_, _ = fmt.Fprintf(out, "creates=%d accepted=%d failed=%d duration=%s\n",
		total, accepted.Load(), failed.Load(), time.Since(start).Round(time.Millisecond))
	if accepted.Load() != int64(total) {
		return 1
	}
	return 0
}

// create sends one notification and checks for 202 with an ID.
func create(client *http.Client, baseURL, channel string, n int) error {
	body, err := json.Marshal(map[string]string{
		"recipient": recipients[channel],
		"channel":   channel,
		"content":   fmt.Sprintf("load %s %d", channel, n),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/notifications", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", fmt.Sprintf("load-%s-%d", channel, n))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s %d: status %d", channel, n, resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		return errors.New(channel + ": 202 without an id")
	}
	return nil
}
