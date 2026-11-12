package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"zenui/internal/zen/lane"
)

// stream:true must put the first token on the wire while the turn is still
// running, not replay the whole answer after the last token arrives.
func TestResponsesStreamerIsIncremental(t *testing.T) {
	rec := httptest.NewRecorder()
	st, err := newResponsesStreamer(rec, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), `"type":"response.created"`) {
		t.Fatal("no response.created on stream open")
	}
	collector := newResponsesCollector()
	chunks := []lane.Chunk{
		{Kind: lane.ChunkBlockStart, Index: 0, BlockType: "text"},
		{Kind: lane.ChunkTextDelta, Index: 0, Delta: "你"},
		{Kind: lane.ChunkTextDelta, Index: 0, Delta: "好"},
		{Kind: lane.ChunkBlockEnd, Index: 0},
	}
	for _, c := range chunks {
		collector.consume(c)
		st.consume(c)
		if c.Kind == lane.ChunkTextDelta && c.Delta == "你" {
			if !strings.Contains(rec.Body.String(), `"delta":"你"`) {
				t.Fatal("first delta was not flushed incrementally")
			}
			if strings.Contains(rec.Body.String(), "response.completed") {
				t.Fatal("completed emitted before the turn finished")
			}
		}
	}
	st.finish(collector, lane.Outcome{})
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"response.output_text.delta"`, `"text":"你好"`,
		`"type":"response.completed"`, "data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream:\n%s", want, body)
		}
	}
}

// Once the 200 is committed a JSON error body is impossible: the failure has
// to travel as an SSE error event with a terminator.
func TestResponsesStreamerFailureIsInStream(t *testing.T) {
	rec := httptest.NewRecorder()
	st, err := newResponsesStreamer(rec, "m")
	if err != nil {
		t.Fatal(err)
	}
	st.fail(&lane.UpstreamError{Message: "上游中途断开"})
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "上游中途断开") {
		t.Fatalf("no error event: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("stream left open after failure")
	}
}
