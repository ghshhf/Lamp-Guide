package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"zenui/internal/zen/lane"
)

// responsesStreamer emits Responses SSE events as chunks arrive. The old
// stream:true path buffered the whole turn in responsesCollector and replayed
// it in one burst: the client saw 200 + response.created only after the last
// token was generated, which is not streaming at all and makes a slow upstream
// look like a hang. writeResponsesEvents is kept for the buffered path.
type responsesStreamItem struct {
	kind        string
	callID      string
	name        string
	eventID     string
	outputIndex int
	text        strings.Builder
}

type responsesStreamer struct {
	sse    *sseWriter
	model  string
	respID string
	items  map[int]*responsesStreamItem
	count  int
}

func newResponsesStreamer(w http.ResponseWriter, model string) (*responsesStreamer, error) {
	se, err := newSSE(w)
	if err != nil {
		return nil, err
	}
	st := &responsesStreamer{
		sse: se, model: model, respID: randomID("resp_"),
		items: map[int]*responsesStreamItem{},
	}
	se.event(map[string]any{"type": "response.created", "response": map[string]any{
		"id": st.respID, "object": "response", "status": "in_progress", "model": model}})
	return st, nil
}

func (st *responsesStreamer) consume(c lane.Chunk) {
	switch c.Kind {
	case lane.ChunkBlockStart:
		it := &responsesStreamItem{kind: c.BlockType, callID: c.ID, name: c.Name, outputIndex: st.count}
		st.count++
		st.items[c.Index] = it
		switch c.BlockType {
		case "text":
			it.eventID = fmt.Sprintf("msg_%d", it.outputIndex)
			st.sse.event(map[string]any{"type": "response.output_item.added", "output_index": it.outputIndex,
				"item": map[string]any{"type": "message", "role": "assistant", "id": it.eventID, "status": "in_progress", "content": []any{}}})
		case "reasoning":
			it.eventID = fmt.Sprintf("rs_%d", it.outputIndex)
			st.sse.event(map[string]any{"type": "response.output_item.added", "output_index": it.outputIndex,
				"item": map[string]any{"type": "reasoning", "id": it.eventID, "summary": []any{}}})
		case "tool-call":
			it.eventID = fmt.Sprintf("fc_%d", it.outputIndex)
			st.sse.event(map[string]any{"type": "response.output_item.added", "output_index": it.outputIndex,
				"item": map[string]any{"type": "function_call", "id": it.eventID, "call_id": c.ID, "name": c.Name, "arguments": "", "status": "in_progress"}})
		default:
			it.eventID = fmt.Sprintf("msg_%d", it.outputIndex)
		}
	case lane.ChunkTextDelta, lane.ChunkReasonDelta, lane.ChunkToolCallDelta:
		it, ok := st.items[c.Index]
		if !ok || c.Delta == "" {
			return
		}
		it.text.WriteString(c.Delta)
		eventType := "response.output_text.delta"
		switch it.kind {
		case "reasoning":
			eventType = "response.reasoning_text.delta"
		case "tool-call":
			eventType = "response.function_call_arguments.delta"
		}
		st.sse.event(map[string]any{"type": eventType, "output_index": it.outputIndex,
			"item_id": it.eventID, "delta": c.Delta})
	case lane.ChunkBlockEnd:
		it, ok := st.items[c.Index]
		if !ok {
			return
		}
		text := it.text.String()
		var item map[string]any
		switch it.kind {
		case "reasoning":
			item = map[string]any{"type": "reasoning", "id": it.eventID, "summary": []any{}}
		case "tool-call":
			args := text
			if args == "" {
				args = "{}"
			}
			item = map[string]any{"type": "function_call", "id": it.eventID, "call_id": it.callID,
				"name": it.name, "arguments": args, "status": "completed"}
		default:
			item = map[string]any{"type": "message", "role": "assistant", "id": it.eventID, "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text}}}
		}
		st.sse.event(map[string]any{"type": "response.output_item.done", "output_index": it.outputIndex, "item": item})
		delete(st.items, c.Index)
	}
}

// finish emits the terminal event. Once the 200 has been committed there is no
// way to send a JSON error body, so a failure travels as an in-stream event.
func (st *responsesStreamer) finish(c *responsesCollector, outcome lane.Outcome) {
	status, event := "completed", "response.completed"
	if outcome.Finish == lane.FinishMaxTokens {
		status, event = "incomplete", "response.incomplete"
	}
	st.sse.event(map[string]any{"type": event, "response": map[string]any{
		"id": st.respID, "object": "response", "status": status, "model": st.model,
		"output": c.outputItems(), "usage": responsesUsage(outcome.Usage)}})
	st.sse.done()
}

func (st *responsesStreamer) fail(uerr *lane.UpstreamError) {
	if uerr == nil {
		return
	}
	st.sse.event(map[string]any{"type": "error", "code": errorType(uerr),
		"message": uerr.Message, "param": nil, "sequence_number": st.count})
	st.sse.done()
}
