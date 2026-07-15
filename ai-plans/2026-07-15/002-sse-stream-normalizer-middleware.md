# Plan: SSE Stream Normalizer Middleware

**Date:** 2026-07-15  
**Status:** Draft  
**Complexity:** High  
**Risk:** Medium  

---

## Problem

llama-swap is an OpenAI-compatible proxy that forwards streaming responses from upstream model servers (llama.cpp, vLLM, etc.) to clients like Zed, LibreChat, VS Code, and custom MCP tools. Each upstream server can produce slightly different SSE chunk shapes:

| Upstream | Known Issues |
|----------|-------------|
| llama.cpp | Missing `system_fingerprint`, some models omit `finish_reason` on tool calls |
| vLLM | Returns `usage: {}` (empty object) in streaming chunks, which breaks strict deserializers |
| LiteLLM proxy | May inject extra fields like `provider`, `cached_tokens` |
| OpenRouter | Adds `provider` block, different chunk boundaries |
| Custom servers | Arbitrary field variations |

Rather than patching each upstream's output format case-by-case, we need a **normalization layer** that guarantees every SSE chunk leaving llama-swap conforms to a strict subset of the OpenAI spec — specifically the subset that strict clients (Zed, LibreChat, Copilot) require.

This is modelled after the pattern found in [Containarium's `filterSSEStream`](https://github.com/FootprintAI/Containarium/blob/76e7e3c86351a419277173d719dfa65c7d711d31/internal/modelgateway/sse.go) (MIT) and [IronClaw's `OpenAiChatChunk`](https://github.com/nearai/ironclaw/blob/f5c649ba31352a46e8e513ec8d5813358fc557b1/src/channels/web/openai_compat.rs) (Apache 2.0).

## Solution: Configurable SSE Normalizer

Add an optional middleware layer between the upstream producer and the client that inspects, normalizes, and guarantees every SSE `data:` line matches the canonical OpenAI streaming format.

### Canonical Output Contract

Every SSE event emitted will conform to:

```json
{
  "id": "chatcmpl-<unique>",
  "object": "chat.completion.chunk",
  "created": <unix_ts>,
  "model": "<resolved_model_name>",
  "choices": [
    {
      "index": 0,
      "delta": {
        "role": "assistant",
        "content": "<text_delta>"
      },
      "finish_reason": null
    }
  ]
}
```

Guarantees:
- `id`, `object`, `created`, `model` **always present** on every chunk
- `choices[].index` **always present**
- `choices[].delta` **always present** (may be empty `{}`)
- `choices[].finish_reason` **always present** (null or string)
- Non-spec fields (`provider`, `cached_tokens`, `extra_content`) **stripped**
- Empty `usage: {}` in non-final chunks **stripped**
- `[DONE]` sentinel **always present** at stream end
- `usage` object on terminal chunk (when available) **preserved**

### Architecture

```
Upstream → SSE scanner (bufio.Scanner) → Normalizer → Client
                                            ↓
                                     Metrics / Capture
                                      (opportunistic)
```

The normalizer reads `data:` lines from the upstream SSE stream, unmarshals them, runs normalization rules, re-marshals, and writes to the output:

```go
type sseNormalizer struct {
    envelope struct {
        id      string
        object  string
        created int64
        model   string
    }
    sawFinish  bool
    sawUsage   bool
}

func (n *sseNormalizer) Normalize(upstreamLine string) string {
    // 1. Parse the upstream SSE line
    // 2. If parse error or [DONE], pass through
    // 3. Capture envelope from first successful chunk
    // 4. Inject missing envelope fields
    // 5. Strip non-spec fields from delta
    // 6. Ensure finish_reason is present
    // 7. Strip empty usage objects
    // 8. Re-marshal and return
}
```

### Configuration

Add to `config.yaml`:

```yaml
upstream:
  normalize_sse: true            # off by default (opt-in)
```

When off, the upstream bytes pass through verbatim (current behavior). When on, the normalizer is active.

### Implementation Phases

#### Phase 1: Core Normalizer

- Parse SSE lines, capture envelope, inject missing fields
- Strip non-spec fields (allowlist-based)
- Strip empty `usage: {}`
- Re-marshal and write

#### Phase 2: Envelope Persistence

- The `id`/`object`/`created`/`model` envelope is the same for all chunks in one response
- Capture from first chunk; inject into all subsequent chunks that lack it
- If the upstream NEVER sends correct envelope (e.g. some bare-bones servers), generate one

#### Phase 3: Strict Mode

Add a stricter mode for the most demanding clients:

```yaml
upstream:
  normalize_sse: strict
```

- All fields required by OpenAI spec are enforced
- `delta.role: "assistant"` injected if missing from first chunk
- Tool-call deltas normalized (index, id/type/function structure)
- `finish_reason: "stop"` injected if missing from terminal chunk
- `usage` object synthesized from metrics data if upstream doesn't provide it

### Changes Required

| File | Change |
|------|--------|
| `internal/server/normalize.go` | **NEW** — SSE normalizer implementation |
| `internal/server/normalize_test.go` | **NEW** — Tests with known upstream edge cases |
| `internal/config/config.go` | Add `NormalizeSSE` field to `UpstreamConfig` |
| `internal/process/process_command.go` | Integrate normalizer into `ModifyResponse` or proxy handler |
| `internal/server/server.go` | Wire config to normalizer construction |

### Testing Strategy

| Test | Method |
|------|--------|
| Missing envelope | Craft upstream chunk w/o id/object/created, verify injection |
| Extra fields | Upstream chunk with `provider: {...}`, verify stripped |
| Empty usage | `usage: {}` in mid-stream chunk, verify removed |
| Finish reason | Terminal chunk without finish_reason, verify injected |
| Tool call delta | Upstream with tool_call, verify index/type/function preserved |
| Real-world capture | Replay captured llama.cpp streams through normalizer |
| Regression | All existing tests pass with normalizer off (default) |

### Success Criteria

- [ ] Upstream chunks with missing envelope fields are repaired
- [ ] Non-spec fields are stripped from output
- [ ] Empty `usage: {}` objects don't reach the client
- [ ] `[DONE]` sentinel is present at stream end (always)
- [ ] Config toggle works: `false` = passthrough, `true` = normalized
- [ ] 100+ real upstream chunk scenarios tested
- [ ] All existing tests pass
