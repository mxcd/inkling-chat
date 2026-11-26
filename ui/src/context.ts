import type { ChatMessage } from './history'

// What fills the context window, derived from the usage of every model call: a call reads what the
// call before it read and wrote, plus what came in since (the next message, the tool results).
export type Part = 'system' | 'input' | 'thinking' | 'output'
export type Row = { label: string; parts: Record<Part, number>; estimated: boolean; live: boolean }

// estimateTokens is a rough count for text the server never counted alone: about four characters
// per token plus the message's framing.
export const estimateTokens = (text: string) => Math.ceil(text.length / 4) + 4

const empty = (): Record<Part, number> => ({ system: 0, input: 0, thinking: 0, output: 0 })

// composition is the system prompt's row, then one row per turn with what the turn added. The
// server counts the system prompt only together with the history and the new message, so the
// system's size is an estimate, taken again whenever a turn switches the file tools (their
// definitions are part of it); everything else comes from the reported usage. The model does not
// read a turn's thinking again, so only the newest turn's thinking counts toward the total. A
// turn's live call adds what was written so far.
export function composition(messages: ChatMessage[]): Row[] {
  const turns: Row[] = []
  const system: Row = { label: 'System', parts: empty(), estimated: true, live: false }
  let systemMode: boolean | null = null
  let history = 0 // tokens of the turns before this one that the model reads again
  let read = 0 // tokens the context held after the last reported call
  let question = ''
  let turn = 0
  for (const message of messages) {
    if (message.role === 'user') {
      question = message.content
      continue
    }
    turn++
    const stats = message.stats
    const parts = empty()
    let estimated = false
    ;(stats?.calls ?? []).forEach((call, index) => {
      let added = Math.max(0, call.inputTokens - read)
      if (index === 0) {
        const asked = estimateTokens(question)
        if (systemMode !== (message.workspace ?? false)) {
          systemMode = message.workspace ?? false
          system.parts.system = Math.max(0, call.inputTokens - history - asked)
          system.label = systemMode ? 'System + file tools' : 'System'
          estimated = true
        }
        // the history the model reads again, not the last call's reply: old thinking is left out
        added = Math.max(0, call.inputTokens - history - system.parts.system)
      }
      parts.input += added
      parts.output += call.outputTokens
      read = call.inputTokens + call.outputTokens
    })
    // reasoning is counted in the output; only the stream tells how much of it was thinking
    const thinking = Math.min(stats?.thinking ?? 0, parts.output)
    parts.thinking += thinking
    parts.output -= thinking
    const live = stats?.live ?? null
    if (live) {
      if (!stats?.calls.length) {
        parts.input += estimateTokens(question)
        estimated = true
      }
      parts.thinking += live.thinking
      parts.output += live.pieces - live.thinking
    }
    if (!stats?.calls.length && !live) continue // a turn that failed before the server counted it
    history += parts.input + parts.output
    turns.push({ label: `Turn ${turn}`, parts, estimated, live: live !== null })
  }
  return systemMode === null ? turns : [system, ...turns]
}

// inWindow is what the rows hold now: everything but the thinking of turns before the newest.
export function inWindow(rows: Row[]): number {
  return rows.reduce((sum, row, index) =>
    sum + row.parts.system + row.parts.input + row.parts.output + (index === rows.length - 1 ? row.parts.thinking : 0), 0)
}

// blockUnit is how many tokens one block stands for: 64, doubled until the tokens in use take at
// most MAXIMUM_BLOCKS blocks, so a context of a million tokens stays a few hundred blocks.
const MAXIMUM_BLOCKS = 512
export function blockUnit(used: number): number {
  let unit = 64
  while (used / unit > MAXIMUM_BLOCKS) unit *= 2
  return unit
}

// blocks is how many units a part takes: any part that is there shows, at least one block.
export const blocks = (tokens: number, unit: number) => (tokens > 0 ? Math.ceil(tokens / unit) : 0)
