package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// A harness that never closes stdin cannot hold the hook past its budget.
func TestReadPayloadBudget(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	go func() { _, _ = pw.Write([]byte(`{"partial":`)) }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	b, err := readPayload(ctx, pr)
	if err == nil || time.Since(t0) > time.Second || string(b) != `{"partial":` {
		t.Fatalf("got %q %v after %v", b, err, time.Since(t0))
	}
	b, err = readPayload(context.Background(), strings.NewReader("{}"))
	if err != nil || string(b) != "{}" {
		t.Fatalf("normal read: %q %v", b, err)
	}
}
