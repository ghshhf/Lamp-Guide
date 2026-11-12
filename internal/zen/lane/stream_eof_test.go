package lane

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamReaderEOFSurvivesSniffAndUnread(t *testing.T) {
	r := newStreamReader(context.Background(), strings.NewReader("short body"), time.Second)
	defer r.Close()
	head := readUpTo(r, 4096, time.Second)
	if string(head) != "short body" {
		t.Fatal(string(head))
	}
	if _, err := r.ReadTimeout(make([]byte, 1), 20*time.Millisecond); !errors.Is(err, io.EOF) {
		t.Fatalf("second read lost EOF: %v", err)
	}
	r.Unread(head)
	all, err := io.ReadAll(r)
	if err != nil || string(all) != "short body" {
		t.Fatalf("unread after EOF: %q %v", all, err)
	}
}

// bufio.ReadString returns the final unterminated line together with io.EOF.
// Dropping it lost the terminal usage/finish frame and made the caller believe
// the upstream never finished, triggering a pointless continuation.
func TestReadSSEKeepsFinalUnterminatedFrame(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}"
	frames := 0
	if _, err := readSSE(strings.NewReader(body), func([]byte) error {
		frames++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if frames != 2 {
		t.Fatalf("final unterminated data: frame was dropped, got %d frames", frames)
	}
}

func TestStreamReaderCancellationUnblocksPendingRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	r := newStreamReader(ctx, reader, time.Hour)
	defer r.Close()
	done := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled reader returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("read ignored cancellation")
	}
}
