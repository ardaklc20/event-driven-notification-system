package main

import (
	"bytes"
	"strings"
	"testing"
)

func start(worker, channel, at string) string {
	return `{"time":"x","level":"INFO","msg":"provider call started","worker_id":"` + worker +
		`","channel":"` + channel + `","started_at":"` + at + `"}`
}

func TestRatecount(t *testing.T) {
	t.Run("counts", func(t *testing.T) {
		in := strings.Join([]string{
			`{"level":"INFO","msg":"worker started","worker_id":"w1"}`,
			start("w1", "sms", "2026-11-20T10:00:00.000Z"),
			start("w2", "sms", "2026-11-20T10:00:00.999Z"),
			start("w2", "sms", "2026-11-20T10:00:01.000Z"),
			`not json at all`,
			start("w1", "email", "2026-11-20T10:00:00.5+00:00"),
		}, "\n")
		var out, errOut bytes.Buffer
		if code := run(strings.NewReader(in), &out, &errOut); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errOut.String())
		}
		want := "channel=email starts=1 max_window=1\nchannel=sms starts=3 max_window=2\nworkers=2\n"
		if out.String() != want {
			t.Fatalf("output %q, want %q", out.String(), want)
		}
	})
	for name, in := range map[string]string{
		"no start line":     `{"msg":"worker started"}`,
		"broken start line": `{"msg":"provider call started","channel":"sms"`,
		"no started_at":     `{"msg":"provider call started","channel":"sms","worker_id":"w1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(strings.NewReader(in), &out, &errOut); code != 1 || errOut.Len() == 0 {
				t.Fatalf("exit %d, stderr %q; want 1 with a message", code, errOut.String())
			}
		})
	}
}
