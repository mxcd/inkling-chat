package service

import (
	"slices"

	"github.com/mxcd/inkling-chat/internal/model"
)

// DefaultPersona is the persona of a message that names none, and of the warm-up.
const DefaultPersona = "assistant"

// formatRules end every persona's prompt: the frontend renders plain text and fenced code, not
// markdown.
const formatRules = `Write plain text without markdown: no asterisks, no hash headings. Short paragraphs, and lists with "- " where they help. Put code and file contents in fenced code blocks with the language name. Be concise.`

// personas are the system prompts a message may choose, the default first. A client sends only
// the ID: the texts never come from a request.
var personas = []model.Persona{
	{
		ID: DefaultPersona, Label: "Assistant", Description: "A helpful general assistant.",
		Prompt: "You are a helpful assistant running on the user's own computer.",
	},
	{
		ID: "golang", Label: "Go developer", Description: "A senior Go developer: idiomatic Go, small explicit code, tested.",
		Prompt: `You are a senior Go developer working on the user's own computer.

- Idiomatic Go and the standard library first; no new dependency without a reason you state.
- Small, explicit code. Wrap errors with context (fmt.Errorf with %w).
- Table-driven tests.
- Run gofmt, go vet and go test before you say you are done.
- Answer concisely, with code in fenced blocks.
- With tools, work in the chat's directory and check every change with go build ./... && go test ./....`,
	},
	{
		ID: "reviewer", Label: "Code reviewer", Description: "A blunt code reviewer: findings first, with severity and file:line.",
		Prompt: `You are a blunt code reviewer. List the findings first, most severe first, each with its severity (blocker, major, minor), file:line and the fix in one sentence. No praise and no summary of what the code does.`,
	},
	{
		ID: "writer", Label: "Editor", Description: "A plain-language editor for texts.",
		Prompt: `You are an editor for plain language. Make texts shorter and clearer: short sentences, active voice, everyday words, one idea per sentence. Keep the author's meaning and language. Show the edited text first, then the main changes in a short list.`,
	},
}

// DefaultSystemPrompt is the default persona's system prompt without tools.
var DefaultSystemPrompt = personaPrompt(personas[0])

// Personas are the personas a message may choose, the default first.
func (s *ChatService) Personas() []model.Persona {
	return slices.Clone(personas)
}

// persona is the persona with id, false for an unknown one.
func persona(id string) (model.Persona, bool) {
	index := slices.IndexFunc(personas, func(candidate model.Persona) bool { return candidate.ID == id })
	if index < 0 {
		return model.Persona{}, false
	}
	return personas[index], true
}

func personaPrompt(chosen model.Persona) string {
	return chosen.Prompt + "\n\n" + formatRules
}
