package minds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// frame is one step of a fake stream: a pause, then an SSE event. A frame
// without an event is a silence; the event cut drops the connection.
type frame struct {
	wait  time.Duration
	event string
	data  string
}

// fakeAPI is a Messages API that answers each request with the next script
// (the last one repeats) and records the request bodies.
type fakeAPI struct {
	scripts     [][]frame
	headerDelay time.Duration // silence before the response headers

	mu       sync.Mutex
	requests []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	script := f.scripts[min(len(f.requests), len(f.scripts)-1)]
	f.requests = append(f.requests, string(body))
	f.mu.Unlock()
	if !pause(r.Context(), f.headerDelay) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	for _, fr := range script {
		if !pause(r.Context(), fr.wait) {
			return
		}
		if fr.event == "cut" {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		if fr.event != "" {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", fr.event, fr.data)
			w.(http.Flusher).Flush()
		}
	}
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAPI) request(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func ev(event, data string) frame { return frame{event: event, data: data} }

func start(input int) frame {
	return ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",`+
		`"model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,`+
		`"usage":{"input_tokens":%d,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, input))
}

func blockStart(i int, block string) frame {
	return ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block))
}

func blockDelta(i int, delta string) frame {
	return ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta))
}

func blockStop(i int) frame {
	return ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
}

func end(stop string, output int) []frame {
	return []frame{
		ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, output)),
		ev("message_stop", `{"type":"message_stop"}`),
	}
}

// pings are n pings, every gap.
func pings(n int, gap time.Duration) []frame {
	f := make([]frame, n)
	for i := range f {
		f[i] = frame{wait: gap, event: "ping", data: `{"type":"ping"}`}
	}
	return f
}

// answerBody is a turn after its message_start: signed thinking, then text.
func answerBody(text string) []frame {
	return append([]frame{
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(0, `{"type":"thinking_delta","thinking":"weighing"}`),
		blockDelta(0, `{"type":"signature_delta","signature":"sig-1"}`),
		blockStop(0),
		blockStart(1, `{"type":"text","text":""}`),
		blockDelta(1, fmt.Sprintf(`{"type":"text_delta","text":%q}`, text)),
		blockStop(1),
	}, end("end_turn", 12)...)
}

func answer(text string) []frame { return append([]frame{start(10)}, answerBody(text)...) }

// toolCall asks for the git tool, its input in a partial JSON delta.
func toolCall() []frame {
	return append([]frame{start(10),
		blockStart(0, `{"type":"tool_use","id":"toolu_1","name":"git","input":{}}`),
		blockDelta(0, `{"type":"input_json_delta","partial_json":"{\"args\":[\"log\"]}"}`),
		blockStop(0),
	}, end("tool_use", 5)...)
}

// paused is a web search the server paused mid-turn, billed in its
// message_delta.
func paused() []frame {
	return []frame{start(10),
		blockStart(0, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`),
		blockDelta(0, `{"type":"input_json_delta","partial_json":"{\"query\":\"go release\"}"}`),
		blockStop(0),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},`+
			`"usage":{"output_tokens":3,"server_tool_use":{"web_search_requests":1}}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	}
}

// A search result and a citation of it, as the API sends them.
const (
	result = `{"type":"web_search_result","url":"https://go.dev/doc/devel/release","title":"Release History",` +
		`"encrypted_content":"enc-1","page_age":"1 week ago"}`
	citation = `{"type":"web_search_result_location","url":"https://go.dev/doc/devel/release","title":"Release History",` +
		`"encrypted_index":"idx-1","cited_text":"go1.26.1 is a minor release"}`
)

// wide is a step with every kind of block a seat replays: signed thinking, a
// web search and its results, cited text and a tool call, the inputs split
// over several partial JSON deltas.
func wide() []frame {
	return append([]frame{start(10),
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(0, `{"type":"thinking_delta","thinking":"weighing"}`),
		blockDelta(0, `{"type":"signature_delta","signature":"sig-1"}`),
		blockStop(0),
		blockStart(1, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`),
		blockDelta(1, `{"type":"input_json_delta","partial_json":"{\"query\":"}`),
		blockDelta(1, `{"type":"input_json_delta","partial_json":"\"go release\"}"}`),
		blockStop(1),
		blockStart(2, `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[`+result+`]}`),
		blockStop(2),
		blockStart(3, `{"type":"text","text":""}`),
		blockDelta(3, `{"type":"citations_delta","citation":`+citation+`}`),
		blockDelta(3, `{"type":"text_delta","text":"Go 1.26.1 is out."}`),
		blockStop(3),
		blockStart(4, `{"type":"tool_use","id":"toolu_1","name":"git","input":{}}`),
		blockDelta(4, `{"type":"input_json_delta","partial_json":"{\"args\":"}`),
		blockDelta(4, `{"type":"input_json_delta","partial_json":"[\"log\"]}"}`),
		blockStop(4),
	}, end("tool_use", 20)...)
}

// wideBuffered is wide as a non-streamed response returns it.
const wideBuffered = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[` +
	`{"type":"thinking","thinking":"weighing","signature":"sig-1"},` +
	`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go release"}},` +
	`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[` + result + `]},` +
	`{"type":"text","text":"Go 1.26.1 is out.","citations":[` + citation + `]},` +
	`{"type":"tool_use","id":"toolu_1","name":"git","input":{"args":["log"]}}],` +
	`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":20}}`

// half is a turn cut after some text.
var half = []frame{start(10), blockStart(0, `{"type":"text","text":""}`), blockDelta(0, `{"type":"text_delta","text":"half"}`)}

type stubTools struct{}

func (stubTools) Tools() []Tool {
	return []Tool{{Name: "git", Description: "git", Properties: map[string]any{"args": map[string]any{"type": "array"}}, Required: []string{"args"}}}
}

func (stubTools) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	return "commit abc", nil
}

// Describe shows the input, so a test tells a call reported with its whole
// input from one reported before its input had streamed in.
func (stubTools) Describe(name string, input json.RawMessage) string {
	return name + " " + string(input)
}

var quick = limits{idle: 300 * time.Millisecond, step: 5 * time.Second, turn: 15 * time.Second, retry: 10 * time.Millisecond}

func testMind(t *testing.T, api *fakeAPI, l limits) *anthropicMind {
	t.Helper()
	return newTestMind(t, api, l, stubTools{}, nil)
}

func newTestMind(t *testing.T, api *fakeAPI, l limits, tools Toolbox, notify func(string)) *anthropicMind {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	spec := Spec{Seat: "opus", Provider: "anthropic", Model: "claude-test", Effort: "low", Search: "off"}
	return anthropicWith(spec, "test-key", Options{MaxTokens: 1000, Tools: tools, Notify: notify}, l, option.WithBaseURL(srv.URL))
}

// notes records what a mind tells the console.
type notes struct {
	mu    sync.Mutex
	lines []string
}

func (n *notes) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, s)
}

// retries returns the retry notices.
func (n *notes) retries() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var r []string
	for _, l := range n.lines {
		if strings.HasPrefix(l, "retrying after ") {
			r = append(r, l)
		}
	}
	return r
}

// countingTools counts the tools it runs.
type countingTools struct {
	stubTools
	runs atomic.Int32
}

func (c *countingTools) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	c.runs.Add(1)
	return c.stubTools.Run(ctx, name, input)
}

func speak(m *anthropicMind, ctx context.Context) (Reply, error) {
	return m.Speak(ctx, "system", []Turn{{Role: "user", Text: "hello"}})
}

// TestStreamedTurn checks a whole streamed turn: the text, the request's
// usage counted once (output from message_delta, not added to message_start's),
// and the signed thinking kept for replay.
func TestStreamedTurn(t *testing.T) {
	api := &fakeAPI{scripts: [][]frame{answer("partition it")}}
	reply, err := speak(testMind(t, api, quick), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "partition it" || reply.Usage.Input != 10 || reply.Usage.Output != 12 || api.count() != 1 {
		t.Fatalf("reply %q, usage %+v, %d requests", reply.Text, reply.Usage, api.count())
	}
	if !strings.Contains(string(reply.Raw), `"signature":"sig-1"`) {
		t.Errorf("the signed thinking was not kept: %s", reply.Raw)
	}
}

// TestAnswerSurvivesALingeringConnection checks that a turn ends at
// message_stop: a connection the server holds open afterwards, silent past
// the idle limit, does not fail the finished answer.
func TestAnswerSurvivesALingeringConnection(t *testing.T) {
	api := &fakeAPI{scripts: [][]frame{append(answer("ok"), frame{wait: 3 * time.Second})}}
	reply, err := speak(testMind(t, api, quick), context.Background())
	if err != nil || reply.Text != "ok" {
		t.Fatalf("reply %q, err %v", reply.Text, err)
	}
}

// TestHeadersCountAsBytes checks that the response headers push the idle
// limit back, so slow headers then a short silence stay under it, and that
// the wait for the headers counts in the turn's longest silence.
func TestHeadersCountAsBytes(t *testing.T) {
	script := append([]frame{{wait: 150 * time.Millisecond}}, answer("ok")...)
	api := &fakeAPI{scripts: [][]frame{script}, headerDelay: 250 * time.Millisecond}
	reply, err := speak(testMind(t, api, quick), context.Background())
	if err != nil || reply.Text != "ok" || api.count() != 1 {
		t.Fatalf("reply %q, err %v after %d requests", reply.Text, err, api.count())
	}
	if reply.Quiet < 250*time.Millisecond {
		t.Errorf("longest silence %s, want the 250ms wait for the headers", reply.Quiet)
	}
}

// TestStreamReplaysLikeBuffered checks that a streamed message goes back on
// the next request exactly as the same message read from a non-streamed
// response would, for every kind of block a seat replays. Fable's history
// check compares what is replayed.
func TestStreamReplaysLikeBuffered(t *testing.T) {
	api := &fakeAPI{scripts: [][]frame{wide(), answer("done")}}
	reply, err := speak(testMind(t, api, quick), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var exchange []json.RawMessage
	if err := json.Unmarshal(reply.Raw, &exchange); err != nil || len(exchange) != 3 {
		t.Fatalf("exchange %s: %v", reply.Raw, err)
	}
	var buffered anthropic.BetaMessage
	if err := json.Unmarshal([]byte(wideBuffered), &buffered); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(anthropicParam(buffered))
	var got, wanted any
	_ = json.Unmarshal(exchange[0], &got)
	_ = json.Unmarshal(want, &wanted)
	if !reflect.DeepEqual(got, wanted) {
		t.Errorf("streamed replay\n%s\nbuffered replay\n%s", exchange[0], want)
	}
}

// TestFallbackServesTheTurn checks a turn a fallback model finished: it is
// billed to that model, which the last fallback block names rather than
// message_start, and its replay leaves out the reasoning of the model that
// declined.
func TestFallbackServesTheTurn(t *testing.T) {
	script := append([]frame{start(10),
		blockStart(0, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(0, `{"type":"signature_delta","signature":"sig-declined"}`),
		blockStop(0),
		blockStart(1, `{"type":"fallback","from":{"model":"claude-test"},"to":{"model":"claude-backup"},`+
			`"trigger":{"type":"refusal","category":"cyber"}}`),
		blockStop(1),
		blockStart(2, `{"type":"thinking","thinking":"","signature":""}`),
		blockDelta(2, `{"type":"signature_delta","signature":"sig-served"}`),
		blockStop(2),
		blockStart(3, `{"type":"text","text":""}`),
		blockDelta(3, `{"type":"text_delta","text":"partition it"}`),
		blockStop(3),
	}, end("end_turn", 12)...)
	reply, err := speak(testMind(t, &fakeAPI{scripts: [][]frame{script}}, quick), context.Background())
	if err != nil || reply.Text != "partition it" {
		t.Fatalf("reply %q, err %v", reply.Text, err)
	}
	if reply.Served != "claude-backup" {
		t.Errorf("served by %q, want the fallback model", reply.Served)
	}
	raw := string(reply.Raw)
	if strings.Contains(raw, "sig-declined") || !strings.Contains(raw, "sig-served") || !strings.Contains(raw, `"type":"fallback"`) {
		t.Errorf("the replay must drop only the declined model's reasoning: %s", raw)
	}
}

// TestStreamFailures checks each way a stream fails for good: an error that
// says why, never a stored turn, every request's input usage counted, and the
// silence that the failure ended noted apart from healthy silence. A stream
// that broke once its response arrived got its one retry, a request that got
// no response the SDK's two, and a time limit none.
func TestStreamFailures(t *testing.T) {
	slowTool := append(pings(2, 200*time.Millisecond), toolCall()...)
	cases := []struct {
		name     string
		api      *fakeAPI
		limits   limits
		want     error
		usage    bool          // whether a message_start arrived, so input usage is known
		requests int           // the requests the turn made; 0 where the tool loop makes it vary
		cut      time.Duration // the silence a failure ended, at least
	}{
		{"cut before message_stop", &fakeAPI{scripts: [][]frame{half}}, quick, errTruncated, true, 2, 0},
		// A dropped connection, as the HTTP/2 health check drops a dead one:
		// the silence before it counts, though the watchdog did not end it. The
		// idle limit is far above the silence, so scheduling delays cannot turn
		// the drop into a stall.
		{"connection lost after a silence", &fakeAPI{scripts: [][]frame{append(half, frame{wait: 250 * time.Millisecond, event: "cut"})}},
			limits{idle: time.Second, step: 5 * time.Second, turn: 15 * time.Second, retry: 10 * time.Millisecond}, io.ErrUnexpectedEOF, true, 2, 200 * time.Millisecond},
		{"silent after the headers", &fakeAPI{scripts: [][]frame{append(half, frame{wait: 3 * time.Second})}}, quick, errStalled, true, 2, quick.idle},
		// The SDK retries a request that got no response twice: a fresh
		// connection may answer where a dead one did not.
		{"silent before the headers", &fakeAPI{scripts: [][]frame{answer("late")}, headerDelay: 3 * time.Second}, quick, errStalled, false, 3, quick.idle},
		{"past the step limit", &fakeAPI{scripts: [][]frame{append(half, pings(60, 100*time.Millisecond)...)}},
			limits{idle: 300 * time.Millisecond, step: time.Second, turn: 15 * time.Second}, errStepTimeout, true, 1, 0},
		{"past the turn limit", &fakeAPI{scripts: [][]frame{slowTool}},
			limits{idle: time.Second, step: 5 * time.Second, turn: time.Second}, errTurnTimeout, true, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := speak(testMind(t, c.api, c.limits), context.Background())
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.usage && reply.Usage.Input < 10 {
				t.Errorf("the request's input usage was lost: %+v", reply.Usage)
			}
			if c.requests > 0 && c.api.count() != c.requests {
				t.Errorf("%d requests, want %d", c.api.count(), c.requests)
			}
			if reply.Cut < c.cut {
				t.Errorf("longest silence a failure ended %s, want at least %s", reply.Cut, c.cut)
			}
			if c.cut > 0 && reply.Quiet >= c.cut {
				t.Errorf("healthy silence %s holds the failure's silence", reply.Quiet)
			}
		})
	}
}

// TestPingsKeepAQuietStreamAlive checks that pings alone, for longer than the
// idle limit, do not trip the watchdog: a thinking model is silent on the wire.
func TestPingsKeepAQuietStreamAlive(t *testing.T) {
	quiet := append(append([]frame{start(10)}, pings(10, 100*time.Millisecond)...), answerBody("ok")...)
	reply, err := speak(testMind(t, &fakeAPI{scripts: [][]frame{quiet}}, quick), context.Background())
	if err != nil || reply.Text != "ok" {
		t.Fatalf("reply %q, err %v", reply.Text, err)
	}
	if reply.Quiet < 50*time.Millisecond || reply.Quiet > quick.idle {
		t.Errorf("longest silence %s, want about the 100ms between pings", reply.Quiet)
	}
}

// streamError is a stream the API fails partway, after some text.
func streamError(kind, message string) []frame {
	return append(half, ev("error", fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, kind, message)))
}

// TestPartwayFailureRetriesOnce checks that a step whose stream fails partway,
// the API overloaded or erring, or the stream broken once its response
// arrived, is retried once, in place: the same request again, the failed
// one's usage still counted and its partial output never stored, the retry
// told to the console and recorded with its reason, and the failure's silence
// kept out of the healthy figure. A second failure, a request the API rejects,
// or an event the SDK cannot decode fails the turn, and the next round reruns
// it.
func TestPartwayFailureRetriesOnce(t *testing.T) {
	overloaded := streamError("overloaded_error", "Overloaded")
	dropped := append(half, frame{event: "cut"})
	cases := []struct {
		name     string
		scripts  [][]frame
		answers  bool
		requests int
		input    int64  // the input usage of the requests that reached message_start
		why      string // the retry's reason; "" for no retry
	}{
		{"overloaded, then an answer", [][]frame{overloaded, answer("ok")}, true, 2, 20, "overloaded_error"},
		{"an API error, then an answer", [][]frame{streamError("api_error", "Internal server error"), answer("ok")}, true, 2, 20, "api_error"},
		{"a dropped connection, then an answer", [][]frame{dropped, answer("ok")}, true, 2, 20, "a broken stream"},
		{"a stall, then an answer", [][]frame{append(half, frame{wait: 3 * time.Second}), answer("ok")}, true, 2, 20, "a stall"},
		{"a stream cut short, then an answer", [][]frame{half, answer("ok")}, true, 2, 20, "a stream cut short"},
		// Between the response headers and message_start, the SDK's retries
		// are over: the step's retry is the only one.
		{"a dropped connection before message_start, then an answer", [][]frame{{{event: "cut"}}, answer("ok")}, true, 2, 10, "a broken stream"},
		{"a stall before message_start, twice", [][]frame{{{wait: 3 * time.Second}}}, false, 2, 0, "a stall"},
		{"overloaded twice", [][]frame{overloaded}, false, 2, 20, "overloaded_error"},
		{"a dropped connection twice", [][]frame{dropped}, false, 2, 20, "a broken stream"},
		{"an invalid request", [][]frame{streamError("invalid_request_error", "Invalid request"), answer("ok")}, false, 1, 10, ""},
		{"an event the SDK cannot decode", [][]frame{append(half, ev("content_block_delta", "{")), answer("ok")}, false, 1, 10, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeAPI{scripts: c.scripts}
			told := &notes{}
			reply, err := speak(newTestMind(t, api, quick, stubTools{}, told.add), context.Background())
			if (err == nil) != c.answers || api.count() != c.requests {
				t.Fatalf("err %v after %d requests, want an answer %v after %d", err, api.count(), c.answers, c.requests)
			}
			if c.answers && (reply.Text != "ok" || strings.Contains(string(reply.Raw), `"half"`)) {
				t.Errorf("reply %q, raw %s: want the retry's answer, without the failed step's text", reply.Text, reply.Raw)
			}
			if reply.Usage.Input != c.input {
				t.Errorf("usage %+v, want an input of %d over the requests", reply.Usage, c.input)
			}
			if c.requests == 2 && api.request(1) != api.request(0) {
				t.Errorf("the retry was not the same request:\n%s\n%s", api.request(0), api.request(1))
			}
			notices := told.retries()
			switch {
			case c.why == "" && (len(notices) > 0 || len(reply.Retries) > 0):
				t.Errorf("no retry wanted, got notices %q and retries %q", notices, reply.Retries)
			case c.why != "" && (len(notices) != 1 || !strings.HasPrefix(notices[0], "retrying after "+c.why+": ")):
				t.Errorf("notices %q, want one retry after %s, with the error", notices, c.why)
			case c.why != "" && (len(reply.Retries) != 1 || !strings.HasPrefix(reply.Retries[0], c.why+": ")):
				t.Errorf("retries %q, want the one after %s recorded with its error", reply.Retries, c.why)
			}
			if c.answers && reply.Quiet >= quick.idle {
				t.Errorf("healthy silence %s holds the failed try's silence", reply.Quiet)
			}
		})
	}
}

// TestRetryRepeatsOnlyTheFailedStep checks that a stream that breaks after
// the turn's first step reruns that step alone: the tool already answered is
// not run again, the same request goes out, and the failed attempt's search,
// which ran, stays in the calls. Its usage counts what arrived: cut after its
// last message_delta, all of it; cut before, only what message_start gave.
func TestRetryRepeatsOnlyTheFailedStep(t *testing.T) {
	searching := []frame{start(10),
		blockStart(0, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`),
		blockDelta(0, `{"type":"input_json_delta","partial_json":"{\"query\":\"go release\"}"}`),
		blockStop(0),
	}
	billed := ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},`+
		`"usage":{"output_tokens":3,"server_tool_use":{"web_search_requests":1}}}`)
	cases := []struct {
		name             string
		broken           []frame
		output, searches int64
	}{
		{"cut after its last message_delta", append(slices.Clone(searching), billed, frame{event: "cut"}), 5 + 3 + 12, 1},
		{"cut before its last message_delta", append(slices.Clone(searching), frame{event: "cut"}), 5 + 1 + 12, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeAPI{scripts: [][]frame{toolCall(), c.broken, answer("done")}}
			tools := &countingTools{}
			reply, err := speak(newTestMind(t, api, quick, tools, nil), context.Background())
			if err != nil || reply.Text != "done" || api.count() != 3 {
				t.Fatalf("reply %q, err %v after %d requests, want the answer after three", reply.Text, err, api.count())
			}
			if api.request(1) != api.request(2) || !strings.Contains(api.request(2), "commit abc") {
				t.Errorf("the retry must resend the failed step, with the tool's result:\n%s\n%s", api.request(1), api.request(2))
			}
			if n := tools.runs.Load(); n != 1 {
				t.Errorf("the tool ran %d times, want once: a retry repeats the step, not the turn", n)
			}
			if reply.Usage.Input != 30 || reply.Usage.Output != c.output || reply.Usage.Searches != c.searches {
				t.Errorf("usage %+v, want 30 in, %d out and %d searches", reply.Usage, c.output, c.searches)
			}
			var exchange []json.RawMessage
			if json.Unmarshal(reply.Raw, &exchange) != nil || len(exchange) != 3 || strings.Contains(string(reply.Raw), "srvtoolu_1") {
				t.Errorf("the stored exchange must hold the tool call, its result and the answer, not the failed step: %s", reply.Raw)
			}
			if !slices.Contains(reply.Calls, `web_search {"query":"go release"}`) {
				t.Errorf("calls %q: the failed attempt's search ran and must stay listed", reply.Calls)
			}
		})
	}
}

// TestChairStopCancelsTheStream checks that the chair's stop ends the turn as
// a cancellation, not a stall, so the floor drops it, and is never retried.
func TestChairStopCancelsTheStream(t *testing.T) {
	api := &fakeAPI{scripts: [][]frame{append(half, pings(50, 100*time.Millisecond)...)}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := speak(testMind(t, api, quick), ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errStalled) || api.count() != 1 {
		t.Fatalf("err = %v after %d requests, want a cancellation after one", err, api.count())
	}
}

// TestToolLoopAndPause checks the loop over streamed steps: a tool call is
// answered with its result, a paused search is resumed by resending it, each
// call is reported with its whole input, and the turn ends on the answer with
// every request's usage counted, the billed search included.
func TestToolLoopAndPause(t *testing.T) {
	api := &fakeAPI{scripts: [][]frame{toolCall(), paused(), answer("done")}}
	reply, err := speak(testMind(t, api, quick), context.Background())
	if err != nil || reply.Text != "done" || api.count() != 3 {
		t.Fatalf("reply %q, err %v, %d requests", reply.Text, err, api.count())
	}
	if !strings.Contains(api.request(1), `"tool_result"`) || !strings.Contains(api.request(1), "commit abc") {
		t.Errorf("the tool result was not sent: %s", api.request(1))
	}
	if !strings.Contains(api.request(1), `"args":["log"]`) {
		t.Errorf("the tool call went back without its input: %s", api.request(1))
	}
	if !strings.Contains(api.request(2), "srvtoolu_1") || !strings.Contains(api.request(2), "go release") {
		t.Errorf("the paused search was not resent whole: %s", api.request(2))
	}
	want := []string{`git {"args":["log"]}`, `web_search {"query":"go release"}`}
	if !slices.Equal(reply.Calls, want) {
		t.Errorf("calls %q, want %q", reply.Calls, want)
	}
	if reply.Usage.Input != 30 || reply.Usage.Output != 5+3+12 || reply.Usage.Searches != 1 {
		t.Errorf("usage %+v, want 30 in, 20 out and the one search over three requests", reply.Usage)
	}
}
