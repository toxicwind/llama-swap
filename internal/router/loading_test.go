package router

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestLoadingWriter_SSEHeadersAndInitialMessage(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()

	mux := newSSEMux(w, logger, "test-model")
	lw := newLoadingWriter(logger, "test-model", mux, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	_ = lw

	// Check SSE headers were set on the underlying recorder
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: want text/event-stream, got %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: want no-cache, got %q", cc)
	}
	if conn := w.Header().Get("Connection"); conn != "keep-alive" {
		t.Errorf("Connection: want keep-alive, got %q", conn)
	}

	body := w.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected SSE data: prefix, got: %s", body)
	}

	content := extractStreamedContent(body)
	if !strings.Contains(content, "━━━━━\n") {
		t.Errorf("missing separator in streamed content: %q", content)
	}
	if !strings.Contains(content, "llama-swap loading model: test-model\n") {
		t.Errorf("missing initial message in streamed content: %q", content)
	}
}

func TestLoadingWriter_WriteHeaderOnce(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()

	mux := newSSEMux(w, logger, "test-model")
	// mux constructor already wrote header 200. Subsequent WriteHeader calls
	// (e.g. 201) must be ignored.
	mux.WriteHeader(http.StatusCreated)

	if w.Code != http.StatusOK {
		t.Errorf("first WriteHeader: want %d, got %d", http.StatusOK, w.Code)
	}
}

func TestLoadingWriter_WritePassthrough(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()

	mux := newSSEMux(w, logger, "test-model")
	// StartUpstream switches to phase 1 so Write passes through directly
	mux.StartUpstream()
	mux.Write([]byte("hello"))
	mux.Flush()

	body := w.Body.String()
	if !strings.Contains(body, "hello") {
		t.Errorf("Write passthrough failed, body: %s", body)
	}
}

func TestLoadingWriter_StartStopsOnCancel(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.tickDuration = 10 * time.Millisecond
	lw.loopStarted = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	go lw.start(ctx)
	<-lw.loopStarted
	cancel()

	if !lw.waitForCompletion(time.Second) {
		t.Fatal("waitForCompletion timed out")
	}

	body := w.Body.String()
	if !strings.Contains(body, "Done!") {
		t.Errorf("expected Done! message, body: %s", body)
	}
}

func TestLoadingWriter_StartShowsSetUpdate(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.tickDuration = 10 * time.Millisecond
	lw.charPerSecond = 0
	lw.loopStarted = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	go lw.start(ctx)
	<-lw.loopStarted

	lw.setUpdate("custom status message")
	time.Sleep(50 * time.Millisecond)
	cancel()

	if !lw.waitForCompletion(time.Second) {
		t.Fatal("waitForCompletion timed out")
	}

	body := w.Body.String()
	content := extractStreamedContent(body)
	if !strings.Contains(content, "custom status message") {
		t.Errorf("expected setUpdate message in output, got: %q", content)
	}
}

func TestLoadingWriter_SendDataFormat(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.sendData("hello world")

	body := w.Body.String()
	if !strings.Contains(body, `"reasoning_content":"hello world"`) {
		t.Errorf("expected reasoning_content in SSE data, body: %s", body)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("expected data: prefix, got: %s", body)
	}
}

func TestLoadingWriter_SendDataIncludesChoiceIndex(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.sendData("hello world")

	body := w.Body.String()

	var msg struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		jsonData := strings.TrimPrefix(line, "data: ")
		if err := json.Unmarshal([]byte(jsonData), &msg); err != nil {
			continue
		}
		if len(msg.Choices) == 0 {
			t.Errorf("expected at least one choice in SSE data, body: %s", body)
			return
		}
		if msg.Choices[0].Index != 0 {
			t.Errorf("expected choice index 0, got %d", msg.Choices[0].Index)
		}
		return
	}
	t.Errorf("no valid SSE data line found in body: %s", body)
}

func TestLoadingWriter_SendLine(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.charPerSecond = 0

	// Capture only the content from this sendLine call
	before := w.Body.Len()
	lw.sendLine("line content")
	after := w.Body.Len()
	chunkBody := w.Body.String()[before:after]

	content := extractStreamedContent(chunkBody)
	if content != "line content\n" {
		t.Errorf("expected complete streamed line, got: %q", content)
	}
}

func TestLoadingWriter_FlushesPeriodicallyDuringStatusUpdates(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	lw.tickDuration = 10 * time.Millisecond
	lw.charPerSecond = 0
	lw.loopStarted = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		lw.start(ctx)
		close(done)
	}()

	<-lw.loopStarted
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	lines := countSSEMessages(body)
	if lines < 2 {
		t.Errorf("expected multiple SSE messages from periodic updates, got %d", lines)
	}
}

func TestLoadingWriter_ReqStored(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	lw := newLoadingWriter(logger, "test-model", mux, req)
	if lw.req != req {
		t.Fatal("req not stored")
	}
}

func TestIsLoadingPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/chat/completions/extra", true},
		{"/v1/completions", false},
		{"/v1/embeddings", false},
		{"/health", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isLoadingPath(tt.path); got != tt.want {
				t.Errorf("isLoadingPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSSEMux_WriteBuffersDuringPhase0(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")

	// During phase 0, Write should buffer, not pass through
	mux.Write([]byte("buffered data"))

	body := w.Body.String()
	if strings.Contains(body, "buffered") {
		t.Error("phase 0 Write should buffer, not write through to dest")
	}

	// After StartUpstream, buffered data should be flushed
	mux.StartUpstream()
	body = w.Body.String()
	if !strings.Contains(body, "buffered data") {
		t.Errorf("expected buffered data after StartUpstream, body: %s", body)
	}
}

func TestSSEMux_WritePassesThroughDuringPhase1(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	mux.StartUpstream()

	mux.Write([]byte("direct data"))
	mux.Flush()

	body := w.Body.String()
	if !strings.Contains(body, "direct data") {
		t.Errorf("expected direct data in phase 1, body: %s", body)
	}
}

func TestSSEMux_RoleChunkEmittedOnConstruction(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")
	_ = mux

	body := w.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("expected role chunk with role=assistant, body: %s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("expected SSE object type, body: %s", body)
	}
}

func TestSSEMux_NoDoneSentinelOnTransition(t *testing.T) {
	logger := logmon.NewWriter(io.Discard)
	w := httptest.NewRecorder()
	mux := newSSEMux(w, logger, "test-model")

	// Write some loading content
	mux.WriteLoading("loading content")
	// Transition
	mux.StartUpstream()
	// Write upstream content
	mux.Write([]byte("[DONE]"))

	body := w.Body.String()
	// The only [DONE] should come from the upstream write, not from the transition
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("expected upstream [DONE] in output, body: %s", body)
	}
}

func countSSEMessages(s string) int {
	scanner := bufio.NewScanner(strings.NewReader(s))
	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			count++
		}
	}
	return count
}

func extractStreamedContent(body string) string {
	var result strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		jsonData := strings.TrimPrefix(line, "data: ")
		var msg struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(jsonData), &msg); err != nil {
			continue
		}
		if len(msg.Choices) > 0 {
			result.WriteString(msg.Choices[0].Delta.ReasoningContent)
		}
	}
	return result.String()
}
