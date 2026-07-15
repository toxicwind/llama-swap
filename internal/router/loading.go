package router

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

var loadingPaths = []string{
	"/v1/chat/completions",
}

func isLoadingPath(path string) bool {
	for _, p := range loadingPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

type loadingWriter struct {
	mux       *sseMux
	req       *http.Request
	ctx       context.Context
	logger    *logmon.Monitor
	modelName string
	startTime time.Time

	pendingMu     sync.Mutex
	pendingUpdate string

	// closed by start when the goroutine finishes (after cleanup messages)
	done chan struct{}

	// test-only: closed when start enters its loop
	loopStarted chan struct{}
	// test-only: override the 1s tick interval
	tickDuration time.Duration
	// test-only: override character streaming speed (0 = no delay)
	charPerSecond float64
}

func newLoadingWriter(logger *logmon.Monitor, modelName string, mux *sseMux, req *http.Request) *loadingWriter {
	now := time.Now()
	s := &loadingWriter{
		mux:           mux,
		req:           req,
		ctx:           req.Context(),
		logger:        logger,
		modelName:     modelName,
		startTime:     now,
		tickDuration:  750 * time.Millisecond,
		charPerSecond: 75,
		done:          make(chan struct{}),
	}

	// Emit initial loading messages.
	// The role-assistant chunk is already emitted by sseMux's constructor.
	s.sendLine("━━━━━")
	s.sendLine("llama-swap loading model: " + modelName)
	return s
}

func (s *loadingWriter) setUpdate(msg string) {
	s.pendingMu.Lock()
	s.pendingUpdate = msg
	s.pendingMu.Unlock()
}

func (s *loadingWriter) start(ctx context.Context) {
	defer close(s.done)

	defer func() {
		// Skip cleanup writes if the client disconnected — the connection
		// is being torn down and flushing against it will panic.
		if s.ctx.Err() != nil {
			return
		}
		duration := time.Since(s.startTime)
		s.sendData("\n")
		s.sendLine(fmt.Sprintf("Done! (%.2fs)", duration.Seconds()))
		s.sendLine("━━━━━")
		s.sendLine(" ")
		// NOTE: deliberately NOT sending [DONE] here because the real
		// upstream response follows on the same SSE stream. A [DONE]
		// sentinel would terminate the stream prematurely, leaving the
		// client (Zed) waiting for chunks that never arrive.
	}()

	remarks := make([]string, len(loadingRemarks))
	copy(remarks, loadingRemarks)
	rand.Shuffle(len(remarks), func(i, j int) {
		remarks[i], remarks[j] = remarks[j], remarks[i]
	})
	ri := 0

	nextRemarkIn := time.Duration(2+rand.Intn(4)) * time.Second
	lastRemarkTime := time.Time{}

	ticker := time.NewTicker(s.tickDuration)
	defer ticker.Stop()

	if s.loopStarted != nil {
		close(s.loopStarted)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pendingMu.Lock()
			update := s.pendingUpdate
			s.pendingUpdate = ""
			s.pendingMu.Unlock()

			if update != "" {
				s.sendData("\n")
				s.sendInline(update)
				s.sendData(" ")
				lastRemarkTime = time.Now()
				nextRemarkIn = time.Duration(5+rand.Intn(5)) * time.Second
			} else if time.Since(lastRemarkTime) >= nextRemarkIn {
				remark := remarks[ri%len(remarks)]
				ri++
				s.sendData("\n")
				s.sendInline(remark)
				s.sendData(" ")
				lastRemarkTime = time.Now()
				nextRemarkIn = time.Duration(5+rand.Intn(5)) * time.Second
			} else {
				s.sendData(".")
			}
		}
	}
}

func (s *loadingWriter) waitForCompletion(timeout time.Duration) bool {
	if s.done == nil {
		return true
	}
	select {
	case <-s.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *loadingWriter) sendInline(text string) {
	chunkSize := 10
	if s.charPerSecond > 0 {
		chunkSize = max(3, int(s.charPerSecond)/15)
	}

	runes := []rune(text)
	for i := 0; i < len(runes); {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[i:end])
		s.sendData(chunk)
		i = end

		if i < len(runes) && s.charPerSecond > 0 {
			time.Sleep(time.Duration(float64(time.Second) * float64(len(chunk)) / s.charPerSecond))
		}
	}
}

func (s *loadingWriter) sendLine(line string) {
	if line == "" {
		s.sendData("\n")
		return
	}
	s.sendInline(line)
	s.sendData("\n")
}

// OpenAI streaming chunk envelope types — matches the spec exactly so Zed's
// strict Rust serde deserializer (ResponseStreamResult untagged enum) can
// parse every chunk. All fields required by spec are present:
//   - id, object, created, model (top-level)
//   - choices[].index, choices[].delta, choices[].finish_reason
//
// Without these, Zed's parser returns "data did not match any variant of
// untagged enum ResponseStreamResult".
type sseDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseEnvelope struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
}

func (s *loadingWriter) sendData(data string) {
	s.mux.WriteLoading(data)
}
