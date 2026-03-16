package groq

import (
	"fmt"
	"strings"
)

// Model holds metadata about a Groq model.
type Model struct {
	ID          string // the string sent to the API
	Nick        string // short alias for fuzzy matching and display
	DisplayName string // human-friendly label
	Description string // one-liner shown to the user (NOT used for matching)
}

// Available lists all supported Groq models.
var Available = []Model{
	{
		ID:          "llama-3.3-70b-versatile",
		Nick:        "llama70b",
		DisplayName: "LLaMA 3.3 70B",
		Description: "Best for coding — default",
	},
	{
		ID:          "llama-3.1-8b-instant",
		Nick:        "llama8b",
		DisplayName: "LLaMA 3.1 8B Instant",
		Description: "Fastest — good for simple questions",
	},
	{
		ID:          "meta-llama/llama-guard-4-12b",
		Nick:        "llamaguard",
		DisplayName: "LLaMA Guard 4 12B",
		Description: "Safety/moderation model",
	},
	{
		ID:          "openai/gpt-oss-120b",
		Nick:        "gptoss",
		DisplayName: "GPT OSS 120B",
		Description: "OpenAI open-source 120B model",
	},
	{
		ID:          "qwen/qwen3-32b",
		Nick:        "qwen3",
		DisplayName: "Qwen3 32B",
		Description: "Qwen reasoning model",
	},
	{
		ID:          "groq/compound",
		Nick:        "compound",
		DisplayName: "Groq Compound",
		Description: "Groq compound model, 70k context",
	},
	{
		ID:          "groq/compound-mini",
		Nick:        "compoundmini",
		DisplayName: "Groq Compound Mini",
		Description: "Groq compound mini model",
	},
}

// DefaultModel is the model used when none is specified.
const DefaultModel = "llama-3.3-70b-versatile"

// Fuzzy finds a model by ID or Nick only — never by Description.
// Match priority:
//  1. Exact Nick match
//  2. Exact ID match
//  3. Nick contains query (substring)
//  4. ID contains query (substring)
//
// Descriptions are intentionally excluded from matching — they contain
// common words ("model", "context", "lightweight", numbers like "120") that
// would cause accidental switches if the user's text happened to contain them.
func Fuzzy(query string) (Model, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Model{}, false
	}

	// Pass 1: exact nick or exact ID.
	for _, m := range Available {
		if strings.ToLower(m.Nick) == q || strings.ToLower(m.ID) == q {
			return m, true
		}
	}

	// Pass 2: nick contains query.
	for _, m := range Available {
		if strings.Contains(strings.ToLower(m.Nick), q) {
			return m, true
		}
	}

	// Pass 3: ID contains query.
	for _, m := range Available {
		if strings.Contains(strings.ToLower(m.ID), q) {
			return m, true
		}
	}

	return Model{}, false
}

// Validate returns an error if id is not in the Available list.
func Validate(id string) error {
	for _, m := range Available {
		if m.ID == id {
			return nil
		}
	}
	ids := make([]string, len(Available))
	for i, m := range Available {
		ids[i] = m.ID
	}
	return fmt.Errorf("unknown model %q — run !model list to see valid models", id)
}
