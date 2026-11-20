// Command wswatch follows one WebSocket and prints each text message as one
// line (K43). It exits with 0 after -for, and with 1 if the connection fails
// or the server closes it first.
//
//	wswatch -url ws://localhost:8080/ws/notifications/<id> -for 30s
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

// readLimit is about five times the largest snapshot, a batch of 1,000
// (README, "Real-time status"). The library's default is 32 KiB.
const readLimit = 1 << 20

func main() {
	url := flag.String("url", "", "WebSocket URL to follow")
	dur := flag.Duration("for", 30*time.Second, "how long to follow it")
	flag.Parse()
	if *url == "" || *dur <= 0 {
		fmt.Fprintln(os.Stderr, "usage: wswatch -url ws://host/ws/notifications/<id> [-for 30s]")
		os.Exit(2)
	}
	os.Exit(run(*url, *dur))
}

func run(url string, dur time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		if resp != nil {
			fmt.Fprintf(os.Stderr, "wswatch: dial: %s: %v\n", resp.Status, err)
		} else {
			fmt.Fprintf(os.Stderr, "wswatch: dial: %v\n", err)
		}
		return 1
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(readLimit)

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0
			}
			if status := websocket.CloseStatus(err); status != -1 {
				fmt.Fprintf(os.Stderr, "wswatch: closed by server: %d %v\n", status, err)
			} else {
				fmt.Fprintf(os.Stderr, "wswatch: read: %v\n", err)
			}
			return 1
		}
		if typ != websocket.MessageText {
			continue
		}
		if _, err := fmt.Fprintf(os.Stdout, "%s\n", data); err != nil {
			return 1
		}
	}
}
