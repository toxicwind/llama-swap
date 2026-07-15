package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// sseMux is a dedicated SSE multiplexer that serializes all writes to the
// underlying http.ResponseWriter. It owns the stream lifecycle:
//
//   - Phase 0 (loading): loading chunks (WriteLoading) are written immediately;
//     upstream proxy writes (Write) are buffered.
//   - Transition (0→1): StartUpstream flushes buffered upstream writes and
//     atomically switches the phase.
//   - Phase 1 (upstream): all writes pass through directly.
//
// sseMux implements http.ResponseWriter and http.Flusher so it can be passed to
// both the loading writer and the real upstream handler without either knowing
// about the other.
type sseMux struct {
	dest      http.ResponseWriter
	log       *logmon.Monitor
	modelName string
	chatID    string
	created   int64

	phase atomic.Int32 // 0=loading, 1=upstream

	mu  sync.Mutex
	buf [][]byte // upstream writes buffered during phase 0

	hasWritten bool // guards WriteHeader once
}

// newSSEMux creates an sseMux, writes SSE headers, emits the initial
// role-assistant chunk, and returns the mux ready for loading content.
func newSSEMux(dest http.ResponseWriter, log *logmon.Monitor, modelName string) *sseMux {
	now := time.Now()
	m := &sseMux{
		dest:      dest,
		log:       log,
		modelName: modelName,
		chatID:    fmt.Sprintf("chatcmpl-%d", now.UnixNano()),
		created:   now.Unix(),
	}

	// SSE headers
	dest.Header().Set("Content-Type", "text/event-stream")
	dest.Header().Set("Cache-Control", "no-cache")
	dest.Header().Set("Connection", "keep-alive")
	dest.WriteHeader(http.StatusOK)

	// Emit the initial role-assistant chunk so strict clients (Zed's untagged
	// ResponseStreamResult) see a well-formed first chunk before any
	// reasoning_content arrives.
	m.writeRoleChunk()

	return m
}

// writeRoleChunk emits a chunk with delta.role="assistant" and no content,
// matching the OpenAI streaming spec where the first chunk announces the role.
func (m *sseMux) writeRoleChunk() {
	type roleDelta struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	}
	type roleChoice struct {
		Index        int       `json:"index"`
		Delta        roleDelta `json:"delta"`
		FinishReason *string   `json:"finish_reason"`
	}
	type roleEnvelope struct {
		ID      string       `json:"id"`
		Object  string       `json:"object"`
		Created int64        `json:"created"`
		Model   string       `json:"model"`
		Choices []roleChoice `json:"choices"`
	}

	msg := roleEnvelope{
		ID:      m.chatID,
		Object:  "chat.completion.chunk",
		Created: m.created,
		Model:   m.modelName,
		Choices: []roleChoice{
			{
				Index:        0,
				Delta:        roleDelta{Role: "assistant"},
				FinishReason: nil,
			},
		},
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		m.log.Errorf("<%s> Failed to marshal role SSE message: %v", m.modelName, err)
		return
	}

	if _, err = fmt.Fprintf(m.dest, "data: %s\n\n", jsonData); err != nil {
		m.log.Debugf("<%s> Failed to write role SSE data: %v", m.modelName, err)
		return
	}
	if flusher, ok := m.dest.(http.Flusher); ok {
		flusher.Flush()
	}
}

// WriteLoading writes a loading chunk (SSE envelope with reasoning_content) to
// the stream. It must only be called during phase 0. If called during phase 1
// the write is silently dropped — the upstream handler owns the stream.
func (m *sseMux) WriteLoading(text string) {
	if m.phase.Load() != 0 {
		return
	}

	msg := sseEnvelope{
		ID:      m.chatID,
		Object:  "chat.completion.chunk",
		Created: m.created,
		Model:   m.modelName,
		Choices: []sseChoice{
			{
				Index: 0,
				Delta: sseDelta{
					ReasoningContent: text,
				},
				FinishReason: nil,
			},
		},
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		m.log.Errorf("<%s> Failed to marshal loading SSE message: %v", m.modelName, err)
		return
	}

	m.mu.Lock()
	_, err = fmt.Fprintf(m.dest, "data: %s\n\n", jsonData)
	m.mu.Unlock()

	if err != nil {
		m.log.Debugf("<%s> Failed to write loading SSE data: %v", m.modelName, err)
		return
	}
	if flusher, ok := m.dest.(http.Flusher); ok {
		flusher.Flush()
	}
}

// StartUpstream transitions the mux from phase 0 (loading) to phase 1
// (upstream). It flushes any buffered upstream writes first, then atomically
// switches the phase so subsequent writes pass through directly.
// No [DONE] sentinel is emitted — the loading and upstream content are part of
// the same SSE stream.
func (m *sseMux) StartUpstream() {
	m.mu.Lock()
	// Flush all buffered upstream writes
	for _, chunk := range m.buf {
		if _, err := m.dest.Write(chunk); err != nil {
			m.log.Debugf("<%s> Failed to write buffered upstream data: %v", m.modelName, err)
			break
		}
	}
	m.buf = nil
	m.mu.Unlock()

	if flusher, ok := m.dest.(http.Flusher); ok {
		flusher.Flush()
	}

	m.phase.Store(1)
}

// Write implements http.ResponseWriter. During phase 0 it buffers the data;
// during phase 1 it passes through directly.
func (m *sseMux) Write(data []byte) (int, error) {
	if m.phase.Load() == 1 {
		return m.dest.Write(data)
	}

	m.mu.Lock()
	chunk := make([]byte, len(data))
	copy(chunk, data)
	m.buf = append(m.buf, chunk)
	m.mu.Unlock()

	return len(data), nil
}

// Header implements http.ResponseWriter.
func (m *sseMux) Header() http.Header {
	return m.dest.Header()
}

// WriteHeader implements http.ResponseWriter. It delegates once to the
// underlying dest, ignoring subsequent calls (the mux constructor already
// wrote 200 OK with SSE headers).
func (m *sseMux) WriteHeader(statusCode int) {
	if m.hasWritten {
		return
	}
	m.hasWritten = true
	m.dest.WriteHeader(statusCode)
}

// Flush implements http.Flusher.
func (m *sseMux) Flush() {
	if flusher, ok := m.dest.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ChatID returns the shared chat completion ID.
func (m *sseMux) ChatID() string { return m.chatID }

// Created returns the Unix timestamp of the stream creation.
func (m *sseMux) Created() int64 { return m.created }
