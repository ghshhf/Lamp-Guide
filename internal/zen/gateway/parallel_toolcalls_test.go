package gateway

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenui/internal/zen/lane"
)

// Chat-wire providers keep tool blocks open until Finish(), so their argument
// fragments can interleave. Every fragment used to be sent with index 0; the
// client then concatenated {"..."}{"..."} into one tool call and failed with
// "Unexpected non-whitespace character after JSON".
func TestStreamParallelToolCallsKeepSeparateIndexes(t *testing.T) {
	e := &chatEmitter{declared: map[string]bool{"pwsh": true}}
	chunks := []lane.Chunk{
		{Kind: lane.ChunkBlockStart, Index: 0, BlockType: "tool-call", ID: "call_1", Name: "pwsh"},
		{Kind: lane.ChunkToolCallDelta, Index: 0, Delta: `{"command":"a"`},
		{Kind: lane.ChunkBlockStart, Index: 1, BlockType: "tool-call", ID: "call_2", Name: "pwsh"},
		{Kind: lane.ChunkToolCallDelta, Index: 1, Delta: `{"command":"b"`},
		{Kind: lane.ChunkToolCallDelta, Index: 0, Delta: `,"description":"x"}`},
		{Kind: lane.ChunkToolCallDelta, Index: 1, Delta: `,"description":"y"}`},
		{Kind: lane.ChunkBlockEnd, Index: 0},
		{Kind: lane.ChunkBlockEnd, Index: 1},
	}
	args := map[int]string{}
	first := map[int]map[string]any{}
	var later []map[string]any
	for _, c := range chunks {
		for _, d := range e.stream(c) {
			calls := d.delta["tool_calls"].([]any)
			for _, raw := range calls {
				call := raw.(map[string]any)
				idx := call["index"].(int)
				fn := call["function"].(map[string]any)
				args[idx] += fn["arguments"].(string)
				if _, seen := first[idx]; !seen {
					first[idx] = call
				} else {
					later = append(later, call)
				}
			}
		}
	}
	if len(args) != 2 {
		t.Fatalf("want 2 distinct tool_calls indexes, got %d: %v", len(args), args)
	}
	for idx, want := range map[int]string{0: "a", 1: "b"} {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args[idx]), &parsed); err != nil {
			t.Fatalf("index %d arguments are not one JSON object: %v (%q)", idx, err, args[idx])
		}
		if parsed["command"] != want {
			t.Errorf("index %d command = %v, want %v", idx, parsed["command"], want)
		}
	}
	if first[0]["id"] != "call_1" || first[1]["id"] != "call_2" {
		t.Errorf("first fragment ids = %v / %v", first[0]["id"], first[1]["id"])
	}
	for _, call := range later {
		for _, key := range []string{"id", "type"} {
			if _, ok := call[key]; ok {
				t.Errorf("later fragment repeats %q: %v", key, call)
			}
		}
		if _, ok := call["function"].(map[string]any)["name"]; ok {
			t.Errorf("later fragment repeats the function name: %v", call)
		}
	}
}

// End-to-end: the same interleaving through the real SSE handler must produce
// two independently parseable arguments strings on the client side.
func TestChatCompletionsStreamParallelToolCalls(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"pwsh","arguments":"{\"command\":\"a\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"pwsh","arguments":"{\"command\":\"b\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"description\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":",\"description\":\"y\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9}}`,
		`[DONE]`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"pwsh","parameters":{"type":"object"}}}]}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	args := map[int]string{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		choices, _ := frame["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			idxFloat, _ := call["index"].(float64)
			fn, _ := call["function"].(map[string]any)
			fragment, _ := fn["arguments"].(string)
			args[int(idxFloat)] += fragment
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %v", len(args), args)
	}
	for idx, raw := range args {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("index %d arguments = %q: %v (two JSON objects concatenated?)", idx, raw, err)
		}
		if parsed["description"] == "" {
			t.Errorf("index %d lost its arguments tail: %v", idx, parsed)
		}
	}
}
