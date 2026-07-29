package astmatrix

import (
	"os"
	"regexp"
	"strings"
)

// provider holds runtime state for one upstream provider.
type provider struct {
	base     string
	keyEnv   string
	keyEnvAlt string
	noAuth   bool
	models   []string
}

// defaultProviders returns the built-in sovereign provider registry.
// It merges the hand-curated core providers with the extended registry
// from free_providers (only openai-format providers with non-empty BaseURL).
func defaultProviders() map[string]*provider {
	providers := map[string]*provider{
		"llama-swap": {
			base:   "http://127.0.0.1:25100/v1",
			noAuth: true,
			models: []string{"local-fast", "local-quality", "local-longctx"},
		},
		"openrouter": {
			base:   "https://openrouter.ai/api/v1",
			keyEnv: "OPENROUTER_API_KEY",
			models: []string{
				// Verified working 2026-07-28
				"google/gemma-4-31b-it:free",
				"google/gemma-4-26b-a4b-it:free",
				"nvidia/nemotron-3-super-120b-a12b:free",
				"nvidia/nemotron-3-nano-30b-a3b:free",
				"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
				"nvidia/nemotron-nano-12b-v2-vl:free",
				"nvidia/nemotron-nano-9b-v2:free",
				"nvidia/nemotron-3-ultra-550b-a55b:free",
				"poolside/laguna-xs-2.1:free",
				"poolside/laguna-s-2.1:free",
				"cohere/north-mini-code:free",
				"openai/gpt-oss-20b:free",
				"inclusionai/ling-3.0-flash:free",
			},
		},
		"nvidia": {
			base:     "https://integrate.api.nvidia.com/v1",
			keyEnv:   "NVIDIA_API_KEY",
			keyEnvAlt: "NVIDIA_NIM_API_KEY",
			models: []string{
				"nvidia/nemotron-3-super-120b-a12b",
				"nvidia/nemotron-3-nano-30b-a3b",
				"meta/llama-3.1-70b-instruct",
				"meta/llama-3.3-70b-instruct",
				"qwen/qwen3.5-397b-a17b",
				"qwen/qwen3.5-122b-a10b",
				"deepseek-ai/deepseek-v4-flash",
				"deepseek-ai/deepseek-v4-pro",
				"mistralai/mistral-large-3-675b-instruct-2512",
				"google/gemma-4-31b-it",
				"z-ai/glm-5.2",
				"thinkingmachines/inkling",
			},
		},
		"groq": {
			base:   "https://api.groq.com/openai/v1",
			keyEnv: "GROQ_API_KEY",
			models: []string{
				"llama-3.3-70b-versatile",
				"qwen/qwen3-32b",
				"qwen/qwen3.6-27b",
				"openai/gpt-oss-120b",
				"openai/gpt-oss-20b",
				"meta-llama/llama-4-scout-17b-16e-instruct",
			},
		},
		"cerebras": {
			base:   "https://api.cerebras.ai/v1",
			keyEnv: "CEREBRAS_API_KEY",
			models: []string{},
		},
		"google": {
			base:   "https://generativelanguage.googleapis.com/v1beta/openai",
			keyEnv: "GOOGLE_API_KEY",
			models: []string{
				"models/gemini-2.5-flash",
				"models/gemini-2.5-flash-lite",
				"models/gemini-2.0-flash",
				"models/gemma-4-31b-it",
			},
		},
		"mistral": {
			base:   "https://api.mistral.ai/v1",
			keyEnv: "MISTRAL_API_KEY",
			models: []string{
				"mistral-small-latest",
				"codestral-latest",
				"mistral-large-latest",
				"mistral-medium-latest",
			},
		},
	}

	// Merge extended providers from registry (skip duplicates, skip non-openai, skip empty BaseURL)
	for id, reg := range RegistryProviders {
		if _, exists := providers[id]; exists {
			continue // core provider takes precedence
		}
		if reg.BaseURL == "" || reg.Format != "openai" {
			continue // can only route openai-format providers
		}
		var models []string
		for _, m := range reg.Models {
			models = append(models, m.ID)
		}
		providers[id] = &provider{
			base:   reg.BaseURL,
			keyEnv: keyEnvFor(id),
			noAuth: reg.NoAuth,
			models: models,
		}
	}
	return providers
}

// codingAlias maps friendly alias -> [provider, model] or nil for auto/fcm.
var codingAlias = map[string][2]string{
	// Auto routing (nil means use strategy)
	"auto": {},
	"fcm":  {},
	// Local-first ranked roles
	"fast":           {"llama-swap", "local-fast"},
	"local-fast":     {"llama-swap", "local-fast"},
	"quality":        {"llama-swap", "local-quality"},
	"local-quality":  {"llama-swap", "local-quality"},
	"longctx":        {"llama-swap", "local-longctx"},
	"local-longctx":  {"llama-swap", "local-longctx"},
	"local-auto":     {"llama-swap", "local-quality"},
	// OpenRouter free aliases (verified working 2026-07-28)
	"gemma4-31b":        {"openrouter", "google/gemma-4-31b-it:free"},
	"gemma4-26b":        {"openrouter", "google/gemma-4-26b-a4b-it:free"},
	"nemotron-super":    {"openrouter", "nvidia/nemotron-3-super-120b-a12b:free"},
	"nemotron-nano":     {"openrouter", "nvidia/nemotron-3-nano-30b-a3b:free"},
	"nemotron-ultra":    {"openrouter", "nvidia/nemotron-3-ultra-550b-a55b:free"},
	"nemotron-omni":     {"openrouter", "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free"},
	"laguna-xs":         {"openrouter", "poolside/laguna-xs-2.1:free"},
	"laguna-s":          {"openrouter", "poolside/laguna-s-2.1:free"},
	"north-mini":        {"openrouter", "cohere/north-mini-code:free"},
	"gpt-oss-20b":       {"openrouter", "openai/gpt-oss-20b:free"},
	"ling-flash":        {"openrouter", "inclusionai/ling-3.0-flash:free"},
	// NVIDIA NIM aliases
	"nim-nemotron-super":    {"nvidia", "nvidia/nemotron-3-super-120b-a12b"},
	"nim-nemotron-nano":     {"nvidia", "nvidia/nemotron-3-nano-30b-a3b"},
	"nim-llama-3.1-70b":     {"nvidia", "meta/llama-3.1-70b-instruct"},
	"nim-llama-3.3-70b":     {"nvidia", "meta/llama-3.3-70b-instruct"},
	"nim-qwen3.5-397b":      {"nvidia", "qwen/qwen3.5-397b-a17b"},
	"nim-qwen3.5-122b":      {"nvidia", "qwen/qwen3.5-122b-a10b"},
	"nim-deepseek-v4-flash":  {"nvidia", "deepseek-ai/deepseek-v4-flash"},
	"nim-deepseek-v4-pro":    {"nvidia", "deepseek-ai/deepseek-v4-pro"},
	"nim-mistral-large-3":    {"nvidia", "mistralai/mistral-large-3-675b-instruct-2512"},
	"nim-gemma4-31b":         {"nvidia", "google/gemma-4-31b-it"},
	"nim-glm5.2":             {"nvidia", "z-ai/glm-5.2"},
	"nim-inkling":            {"nvidia", "thinkingmachines/inkling"},
	// Google aliases
	"gemini-2.5-flash":      {"google", "models/gemini-2.5-flash"},
	"gemini-2.5-flash-lite": {"google", "models/gemini-2.5-flash-lite"},
	"gemini-2.0-flash":      {"google", "models/gemini-2.0-flash"},
	"gemma4-31b-google":     {"google", "models/gemma-4-31b-it"},
	// Mistral aliases
	"mistral-small":  {"mistral", "mistral-small-latest"},
	"codestral":      {"mistral", "codestral-latest"},
	"mistral-large":  {"mistral", "mistral-large-latest"},
	"mistral-medium": {"mistral", "mistral-medium-latest"},
	// Groq aliases
	"groq-llama-3.3-70b":   {"groq", "llama-3.3-70b-versatile"},
	"groq-qwen3-32b":       {"groq", "qwen/qwen3-32b"},
	"groq-qwen3.6-27b":     {"groq", "qwen/qwen3.6-27b"},
	"groq-gpt-oss-120b":    {"groq", "openai/gpt-oss-120b"},
	"groq-gpt-oss-20b":     {"groq", "openai/gpt-oss-20b"},
	"groq-llama-4-scout":   {"groq", "meta-llama/llama-4-scout-17b-16e-instruct"},
	// Extended provider aliases from registry
	"opencode":              {"opencode", "opencode"},
	"xai-grok-4":            {"xai", "grok-4"},
	"xai-grok-3":            {"xai", "grok-3"},
	"mimo-auto":             {"mimo-free", "mimo-auto"},
	"perplexity-sonar":      {"perplexity", "sonar-pro"},
	"together-llama-3.3":    {"together", "meta-llama/Llama-3.3-70B-Instruct-Turbo"},
}

// localPatterns matches known local GGUF model ID prefixes.
var localPatterns = regexp.MustCompile(`^(beellama|mradermacher|jackrong|turboquant|ik_llama|ik_turboquant|holo|qwen/|gemma-4|exaone)`)

// isLocalSwapModelId returns true for model IDs that should route to the local llama-swap.
func isLocalSwapModelId(model string) bool {
	if model == "" || model == "auto" || model == "fcm" {
		return false
	}
	if target, ok := codingAlias[model]; ok && len(target) > 0 && target[0] == "llama-swap" {
		return true
	}
	return localPatterns.MatchString(model)
}

// resolveModel resolves a model name to (provider, modelID).
func resolveModel(model string, providers map[string]*provider) (string, string) {
	// Check coding aliases
	if target, ok := codingAlias[model]; ok {
		if len(target) > 0 && target[0] != "" {
			return target[0], target[1]
		}
	}
	// Check if it's a local GGUF model ID
	if isLocalSwapModelId(model) {
		return "llama-swap", model
	}
	// Auto/fcm: search all providers by weighted ELO
	if model == "auto" || model == "fcm" {
		// Find first provider with a key and circuit ok
		for pname, p := range providers {
			if pname == "llama-swap" {
				continue
			}
			if p.noAuth || os.Getenv(p.keyEnv) != "" {
				if len(p.models) > 0 {
					return pname, p.models[0]
				}
			}
		}
		return "llama-swap", "local-quality"
	}
	// Search provider model lists
	for pname, p := range providers {
		for _, m := range p.models {
			if m == model {
				return pname, model
			}
		}
	}
	// Fallback: openrouter -> nvidia -> llama-swap
	if _, ok := providers["openrouter"]; ok {
		return "openrouter", model
	}
	if _, ok := providers["nvidia"]; ok {
		return "nvidia", model
	}
	return "llama-swap", "local-quality"
}

// isExplicit returns true if the model maps to a specific provider via coding aliases.
func isExplicit(model string) bool {
	target, ok := codingAlias[model]
	if !ok {
		return false
	}
	return len(target) > 0 && target[0] != ""
}

// astRe detects code/AST content in responses.
var astRe = regexp.MustCompile(`(def |class |import |from |function |const |let |var |#include|package |fn |pub |struct |impl |async |await |\.ts|\.py|\.rs|\.js|AST|tree-sitter|syntax|` + "```" + `)`)

func isAST(text string) bool {
	if text == "" {
		return false
	}
	if strings.Contains(text, "```") {
		return true
	}
	sample := text
	if len(sample) > 5000 {
		sample = sample[:5000]
	}
	return astRe.MatchString(sample)
}
