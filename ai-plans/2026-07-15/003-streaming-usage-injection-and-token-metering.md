# Plan: Streaming Usage Injection & Token Metering

**Date:** 2026-07-15  
**Status:** Draft  
**Complexity:** High  
**Risk:** Low  

---

## Problem

OpenAI's streaming API supports `stream_options: {"include_usage": true}` which tells the server to emit a final chunk containing token usage statistics:

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion.chunk",
  "created": 1234567890,
  "model": "model-name",
  "choices": [],
  "usage": {
    "prompt_tokens": 42,
    "completion_tokens": 17,
    "total_tokens": 59
  }
}
```

Many clients (Zed, LibreChat, VS Code Copilot) depend on this usage chunk for **token accounting and UI display**. Without it:

- Zed's assistant panel shows `Used 0 tokens` or nothing
- LibreChat cannot display per-conversation token usage
- Copilot cannot report API consumption
- The activity store (`store.db`) records 0 tokens for streaming requests

Additionally, llama-swap's current loading writer **always reports `usage: null`** because it has no real token data, and the upstream model's usage is not captured for streaming requests — it's only captured for non-streaming responses where the full JSON body is buffered.

## Solution: Multi-Layer Usage Pipeline

### Layer 1: Upstream Usage Passthrough

Inject `stream_options.include_usage = true` into every streaming `/v1/chat/completions` request before forwarding to the upstream model. This signals the upstream (llama.cpp, vLLM, etc.) to emit a final `usage` chunk.

When the upstream emits this chunk:
- It passes through the SSE normalizer (plan 002)
- The normalizer captures the usage values
- The usage block is forwarded verbatim to the client

**Implementation:** Simple body rewrite in `internal/server/filters.go`:

```go
func injectStreamOptions(body []byte) ([]byte, error) {
    // Add "stream_options": {"include_usage": true} to JSON body
    // if stream: true and stream_options is not already set
}
```

### Layer 2: Usage Synthesis (When Upstream Doesn't Support It)

Not all upstream servers support `stream_options.include_usage`. For those that don't (older llama.cpp, custom servers), llama-swap **synthesizes** a usage chunk from its own metrics data.

llama-swap already tracks:
- Input tokens (from the request tokenization)
- Output tokens (accumulated across streaming chunks)
- Model name (for rate calculations)
- Duration (from request timing)

When the upstream sends its terminal chunk (with `finish_reason`), the synthesizer injects:

```go
type usageSynthesizer struct {
    inputTokens     int  // from request, or estimated
    accumulated     int  // character / token count from delta content
    finishReason    string
    model           string
}
```

### Layer 3: Metering for Loading State

The loading writer emits synthetic chunks that contain zero real token data. After the loading preamble ends and the real upstream response begins, the synthesizer captures the upstream's actual usage.

For the loading writer specifically:
- Parse `reasoning_content` text length and record as a "loading overhead" metric
- Do NOT include this in the final `usage` object sent to the client
- Only real upstream token counts go into the usage chunk

### Layer 4: Activity Log Enhancement

The `metricsMonitor` currently records activity for streaming requests with `inTokens` and `outTokens` both set to 0 (followed by "recording minimal metrics"). With usage injection, the activity log can record real token counts:

| Field | Current | Fixed |
|-------|---------|-------|
| `inTokens` | 0 | Captured from upstream usage chunk |
| `outTokens` | 0 | Captured from upstream usage chunk |
| `totalTokens` | 0 | Sum of in + out |
| `finishReason` | "" | Captured from terminal chunk |

### Configuration

```yaml
metrics:
  enable_usage_injection: true     # inject stream_options (Layer 1)
  synthesize_usage: true           # synthesize when upstream doesn't (Layer 2)
  record_loading_overhead: false   # include loading text in metrics (Layer 3)
```

### Changes Required

| File | Change |
|------|--------|
| `internal/server/filters.go` | Add `injectStreamOptions` to body filter middleware |
| `internal/server/metrics.go` | Add usage capture from streaming terminal chunks |
| `internal/server/normalize.go` | Integrate usage synthesis into normalizer |
| `internal/router/loading.go` | Report loading character count for metrics (optional) |
| `internal/config/config.go` | Add `EnableUsageInjection`, `SynthesizeUsage` config fields |
| `internal/server/server_test.go` | Test usage injection in streaming responses |

### Flow Diagram

```
Client Request (stream=true)
    │
    ▼
Filter Middleware
    │
    ├── injects stream_options.include_usage=true
    │
    ▼
Upstream Server
    │
    ├── Emits content chunks (delta.content)
    ├── Emits terminal chunk (finish_reason)
    └── Emits usage chunk (if supported) ← NEW passthrough
    │
    ▼
SSE Normalizer (Plan 002)
    │
    ├── Captures usage from upstream chunk
    ├── Synthesizes usage if missing ← Layer 2
    │
    ▼
Client
    │
    ├── Receives content + finish_reason
    └── Receives usage chunk ← NEW
```

### Testing Strategy

| Scenario | Verification |
|----------|-------------|
| Upstream with `include_usage` | Usage chunk reaches client verbatim |
| Upstream WITHOUT usage | Synthesized usage chunk matches token counts |
| Loading state + real usage | Loading overhead excluded from final usage |
| Non-streaming request | Unchanged (already captured via JSON body) |
| Metrics recording | Activity log shows correct in/out/total tokens |
| Config disabled | No injection, passthrough, or synthesis |

### Success Criteria

- [ ] `stream_options.include_usage` injected into all streaming requests
- [ ] Upstream usage chunks forwarded to client (if present)
- [ ] Synthesized usage chunks emitted when upstream doesn't support it
- [ ] Token counts in activity log are non-zero for streaming requests
- [ ] Loading state text is excluded from usage tokens
- [ ] `"error processing streaming response: no valid JSON data found in stream"` warning disappears for well-formed responses
- [ ] Backward compatible: all existing tests pass
- [ ] Existing captures in `store.db` are readable (no schema change needed)
