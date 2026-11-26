// The thinking a message asks for: how long the model reasons before it answers. Off answers at
// once; the levels are the model server's reasoning efforts.
export const THINKING_LEVELS = ['off', 'low', 'medium', 'high'] as const
export type Thinking = (typeof THINKING_LEVELS)[number]

export const THINKING_LABELS: Record<Thinking, string> = { off: 'Off', low: 'Low', medium: 'Medium', high: 'High' }

const KEY = 'inkling-chat.thinking'

const isThinking = (value: unknown): value is Thinking => THINKING_LEVELS.includes(value as Thinking)

// loadThinking is the last choice of this browser, off without one.
export const loadThinking = (storage: Storage = localStorage): Thinking => {
  const kept = storage.getItem(KEY)
  return isThinking(kept) ? kept : 'off'
}

export const saveThinking = (thinking: Thinking, storage: Storage = localStorage) => storage.setItem(KEY, thinking)

// withReasoning adds a piece of the streamed thinking to an answer's; the thinking of a model call
// after a tool's result starts a paragraph of its own.
export const withReasoning = (reasoning: string | undefined, piece: string, newCall: boolean) =>
  reasoning && newCall ? `${reasoning.trimEnd()}\n\n${piece}` : (reasoning ?? '') + piece
