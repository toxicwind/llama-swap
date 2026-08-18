#!/bin/bash
# =============================================================================
# MAXIMAL DEV SANDBOX — KIMI + ALL MODELS
# =============================================================================
set -euo pipefail

REPO="${HOME}/projects/llama-swap"
cd "$REPO"

echo "=== [1/6] BUILD ==="
go build ./...

echo ""
echo "=== [2/6] UNIT TESTS ==="
go test ./internal/astmatrix/... -v -run TestCircuitBreaker
go test ./internal/astmatrix/... -v -run TestRateLimiting
go test ./internal/astmatrix/... -v -run TestCoalescing
go test ./internal/astmatrix/... -v -run TestHealthDB

echo ""
echo "=== [3/6] START MOCK SERVER ==="
cat > /tmp/mock_server.go << 'GOEOF'
package main
import (
    "encoding/json"
    "net/http"
)
func main() {
    http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
        var req map[string]interface{}
        json.NewDecoder(r.Body).Decode(&req)
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "id": "mock-123",
            "model": req["model"],
            "choices": []map[string]interface{}{
                {"message": map[string]string{"role": "assistant", "content": "Hello from mock server"}},
            },
        })
    })
    http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(200)
        w.Write([]byte(`{"status":"ok"}`))
    })
    println("Mock server on :9999")
    http.ListenAndServe(":9999", nil)
}
GOEOF
go run /tmp/mock_server.go &
MOCK_PID=$!
sleep 2

echo ""
echo "=== [4/6] INTEGRATION TEST ==="
go test ./internal/astmatrix/... -v -run TestLiveKimi -timeout 120s

echo ""
echo "=== [5/6] LIVE TEST (requires API keys) ==="
if [ -n "${KIMI_API_KEY:-}" ] || [ -n "${OPENROUTER_API_KEY:-}" ] || [ -n "${GROQ_API_KEY:-}" ]; then
    LIVE_TEST=1 go test ./internal/astmatrix/... -v -run TestLiveKimi -timeout 300s
else
    echo "SKIP: No API keys set. Export KIMI_API_KEY, OPENROUTER_API_KEY, or GROQ_API_KEY"
fi

kill $MOCK_PID 2>/dev/null || true

echo ""
echo "=== [6/6] BENCHMARK ==="
go test ./internal/astmatrix/... -bench=. -benchmem

echo ""
echo "=== SANDBOX COMPLETE ==="
echo ""
echo "To run live tests with KIMI:"
echo "  export KIMI_API_KEY=your_key"
echo "  export KIMI_API_URL=https://kimi-api-sandbox.msh.team/v1"
echo "  LIVE_TEST=1 go test ./internal/astmatrix/... -v -run TestLiveKimi"
