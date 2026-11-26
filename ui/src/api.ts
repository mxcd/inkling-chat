// The chat API's contract, written by hand: one streaming route and the personas (API-011 is
// waived in .govengers.yml).
import type { Thinking } from './thinking'

export type ToolCallEvent = { kind: 'tool_call'; callId: string; tool: string; target?: string }
export type ToolResultEvent = { kind: 'tool_result'; callId: string; tool: string; ok?: boolean; error?: string }
// Usage is what one model call cost: inputTokens include the cachedInputTokens, outputTokens the
// reasoning; latencyMs is the time to the first token as the app's server saw it. contextLength is
// the model's context window, 0 when the model server does not name it. The prompt and predicted
// fields and the draft counts are the model server's own measurement, 0 (or missing in older
// history) when it sends none: promptTokens is what it read, its cached tokens not counted.
export type Usage = {
  contextLength: number
  inputTokens: number
  cachedInputTokens: number
  outputTokens: number
  latencyMs: number
  durationMs: number
  tokensPerSecond: number
  promptMs?: number
  promptTokens?: number
  promptPerSecond?: number
  predictedPerSecond?: number
  draftTokens?: number
  draftAccepted?: number
}
export type ChatEvent =
  | { kind: 'text'; text: string }
  | { kind: 'thinking'; text: string }
  | { kind: 'usage'; usage: Usage }
  | ToolCallEvent
  | ToolResultEvent
  | { kind: 'end'; reason: string }
  | { kind: 'error'; code: string; message: string }

// parseFrames takes the complete server-sent events out of buffer and returns them with the
// unfinished rest, which the next chunk continues.
export function parseFrames(buffer: string): { events: ChatEvent[]; rest: string } {
  const events: ChatEvent[] = []
  let rest = buffer
  for (;;) {
    const end = rest.indexOf('\n\n')
    if (end < 0) return { events, rest }
    const frame = rest.slice(0, end)
    rest = rest.slice(end + 2)
    const kind = /^event: (.*)$/m.exec(frame)?.[1]
    const data = /^data: (.*)$/m.exec(frame)?.[1]
    if (kind && data) events.push({ kind, ...JSON.parse(data) } as ChatEvent)
  }
}

export class ChatError extends Error {}

// Persona is a system prompt a message may choose by its id.
export type Persona = { id: string; label: string; description: string }

// DirectoryListing is a folder the tools may work in: its path, its parent (empty at a root),
// the roots and its subfolders' names.
export type DirectoryListing = { path: string; parent: string; roots: string[]; directories: string[] }

// listDirectories is the folder at path, the first root for an empty one. It throws a ChatError
// with the server's text for a folder the chat may not use.
export async function listDirectories(path: string): Promise<DirectoryListing> {
  const response = await fetch(`/api/v1/directories?path=${encodeURIComponent(path)}`)
  const answer = (await response.json().catch(() => null)) as (DirectoryListing & { message?: string }) | null
  if (!response.ok || !answer) throw new ChatError(answer?.message ?? 'The server did not list the folder.')
  return answer
}

// listPersonas is the server's personas, the default first.
export async function listPersonas(): Promise<Persona[]> {
  const response = await fetch('/api/v1/personas')
  if (!response.ok) throw new ChatError('The server did not list the personas.')
  return ((await response.json()) as { items: Persona[] }).items
}

// streamChat posts one message of a conversation and hands every event of the answer to onEvent.
// It throws a ChatError with a text for the reader when the server refuses the message or the
// turn fails.
export async function streamChat(
  conversationId: string,
  message: { text: string; workspace: boolean; directory: string; thinking: Thinking; persona: string },
  onEvent: (event: ChatEvent) => void,
  signal: AbortSignal,
) {
  const response = await fetch(`/api/v1/conversations/${conversationId}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(message),
    signal,
  })
  if (!response.ok || !response.body) {
    const refused = (await response.json().catch(() => null)) as { message?: string } | null
    throw new ChatError(refused?.message ?? 'The server did not answer.')
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) return
    const parsed = parseFrames(buffer + decoder.decode(value, { stream: true }))
    buffer = parsed.rest
    for (const event of parsed.events) {
      if (event.kind === 'error') throw new ChatError(event.message)
      onEvent(event)
    }
  }
}
