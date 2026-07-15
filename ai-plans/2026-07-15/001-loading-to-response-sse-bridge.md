# Plan: Loading-to-Response SSE Bridge (Graceful Handoff)

**Date:** 2026-07-15  
**Status:** Draft  
**Complexity:** Medium  
**Risk:** Low  

---

## Problem

The current loading-writer architecture interleaves two independent SSE producers on the same HTTP response stream:

1. **Loading writer** — synthetic SSE chunks with loading status text (via `reasoning_content`)
2. **Upstream proxy** — real model chunks passthrough via `httputil.ReverseProxy`

After `finishLoading()` completes, the loading goroutine is cancelled, the writer is released, and `resp.HandleFunc(w, req)` runs the real handler — writing directly to the same `http.ResponseWriter`. This means:

- The SSE stream contains loading preamble chunks followed by real chunks with NO separation
- There is no `[DONE]` sentinel between them (because that would terminate the stream)
- Strict clients see a single SSE stream with heterogenous content
- If the loading writer's deferred cleanup writes after the real handler starts (race), it panics

## Solution: SSE Multiplexer with Handoff

Replace the raw `http.ResponseWriter` sharing with a proper SSE multiplexer that owns the stream and accepts content from either producer:

### Architecture

```
Client ←── SSE Mux (owns the stream)
               ├── LoadingProducer  (synthetic status chunks)
               └── UpstreamProducer (real model chunks, buffered until handoff)
```

The SSE Mux:

1. **Phase 1 — Loading**: Passes loading chunks through to the client. Keeps the upstream's first chunk buffered until handoff.
2. **Phase 2 — Handoff**: When the upstream produces its first real chunk, the Mux sends a clean `[DONE]` to terminate any loading state, then streams the buffered + subsequent upstream chunks. The client sees loading, then `[DONE]`, then a new stream of real chunks.
3. **Phase 3 — Upstream only**: Pure passthrough for all remaining chunks.

### Key Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Stream termination | Send `[DONE]` between loading and real | Clean separation; clients reset their parser state |
| First chunk buffering | Buffer until `finishLoading` or first real chunk | Prevents stale loading chunk from corrupting real stream |
| Error handling | Mux emits `data: [DONE]` on upstream error | Clean termination even on failure |
| Backpressure | Channel-backed with bounded buffer (64 slots) | Loading goroutine won't block on slow clients |

### Implementation Outline

**New file:** `internal/router/stream.go`

```go
// sseMux owns an SSE stream and routes content from multiple producers.
type sseMux struct {
    writer    http.ResponseWriter
    log       *logmon.Monitor
    modelName string
    phase     atomic.Int32 // 0=loading, 1=handoff, 2=upstream
    upstreamCh chan []byte // buffered upstream chunks (capacity 64)
    done       chan struct{}
}
```

**Modified flow in `base.go` `ServeHTTP`:**

```go
// Before: writer sharing
// After: mux creation
mux := newSSEMux(w, logger, modelName)
lw := newLoadingWriter(mux, ...)  // loading writes to mux, not raw w

go func() {
    // Real handler writes to mux
    resp.HandleFunc(mux, req)
    mux.Close()
}()

// Loading runs on mux.Phase(0)
// When lw.start returns, mux transitions to Phase(1/2)
```

### Changes Required

| File | Change |
|------|--------|
| `internal/router/stream.go` | **NEW** — SSE multiplexer with handoff logic |
| `internal/router/loading.go` | Accept `io.Writer` instead of `http.ResponseWriter`; remove direct SSE write/flush |
| `internal/router/base.go` | Create mux, pass it to both loading and real handlers |
| `internal/router/loading_test.go` | Update to use mux interface |

### Testing Strategy

- **Unit**: Create `sseMux` with `httptest.NewRecorder`, simulate loading + upstream producers, verify output ordering
- **Integration**: Use fake model with load delay, verify stream output contains loading chunks + `[DONE]` + real chunks
- **Edge cases**: Upstream error during loading, client disconnect during handoff, empty loading preamble

### Success Criteria

- [ ] Loading chunks appear before `[DONE]`
- [ ] Real chunks appear after `[DONE]`
- [ ] No race conditions in stream writes (verified by `-race` tests)
- [ ] Backpressure never blocks the loading goroutine
- [ ] Client disconnect handled gracefully (no hanging goroutines)
- [ ] All existing tests pass
