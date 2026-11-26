import type { Usage } from './api'

// LiveCall is the model call that runs right now, as this browser sees it: startedAt is when it
// was sent, firstTokenAt when its first piece arrived. The server sends one piece per token, so
// pieces count the tokens written so far, reasoning included.
export type LiveCall = { startedAt: number; firstTokenAt: number | null; lastTokenAt: number | null; pieces: number; thinking: number }

// TurnStats is the expert view of one answer: the usage the server reported for every model call
// of the turn, the call that still runs, and the reasoning pieces of the reported calls (the
// server counts them as output; only the stream tells them apart).
export type TurnStats = { calls: Usage[]; live: LiveCall | null; thinking?: number }

const newCall = (now: number): LiveCall => ({ startedAt: now, firstTokenAt: null, lastTokenAt: null, pieces: 0, thinking: 0 })

// Below this much writing time a rate is noise.
const MINIMUM_RATE_MS = 250

// TurnMeter follows the model calls of one turn as its events arrive (times in milliseconds of
// performance.now()). A call starts with the message and after the tool results it answers.
export class TurnMeter {
  readonly stats: TurnStats

  constructor(stats: TurnStats, now: number) {
    this.stats = stats
    stats.live = newCall(now)
  }

  // piece counts one piece of text, or of reasoning when thinking.
  piece(now: number, thinking = false) {
    const live = (this.stats.live ??= newCall(now))
    live.firstTokenAt ??= now
    live.lastTokenAt = now
    live.pieces++
    if (thinking) live.thinking++
  }

  // usage ends the running call with what the server measured.
  usage(usage: Usage) {
    this.stats.calls.push(usage)
    this.stats.thinking = (this.stats.thinking ?? 0) + (this.stats.live?.thinking ?? 0)
    this.stats.live = null
  }

  // toolResult starts the next call: the model is asked again once its tools ran. The last
  // result of a reply wins.
  toolResult(now: number) {
    this.stats.live = newCall(now)
  }

  // finish drops a call the server reported no usage for (a stopped or failed turn).
  finish() {
    this.stats.live = null
  }
}

// LiveView is the running call at now: still processing the prompt, or writing at a rate.
export type LiveView =
  | { phase: 'prompt'; elapsedMs: number }
  | { phase: 'writing'; latencyMs: number; tokens: number; tokensPerSecond: number | null; elapsedMs: number }

export function liveView(live: LiveCall, now: number): LiveView {
  if (live.firstTokenAt === null) return { phase: 'prompt', elapsedMs: now - live.startedAt }
  const writingMs = (live.lastTokenAt ?? now) - live.firstTokenAt
  return {
    phase: 'writing',
    latencyMs: live.firstTokenAt - live.startedAt,
    tokens: live.pieces,
    // pieces after the first over the time they took: the first marks the start of writing
    tokensPerSecond: writingMs >= MINIMUM_RATE_MS && live.pieces > 1 ? ((live.pieces - 1) / writingMs) * 1000 : null,
    elapsedMs: now - live.startedAt,
  }
}

// Totals sums the reported calls of a turn; the rate is all output over all writing time.
export type Totals = Usage & { calls: number }

export function totals(calls: Usage[]): Totals {
  const sum: Totals = { calls: calls.length, contextLength: calls.at(-1)?.contextLength ?? 0, inputTokens: 0, cachedInputTokens: 0, outputTokens: 0, latencyMs: 0, durationMs: 0, tokensPerSecond: 0 }
  let writingMs = 0
  for (const call of calls) {
    sum.inputTokens += call.inputTokens
    sum.cachedInputTokens += call.cachedInputTokens
    sum.outputTokens += call.outputTokens
    sum.latencyMs += call.latencyMs
    sum.durationMs += call.durationMs
    if (call.tokensPerSecond > 0) writingMs += call.durationMs - call.latencyMs
  }
  const written = calls.reduce((tokens, call) => tokens + (call.tokensPerSecond > 0 ? call.outputTokens : 0), 0)
  sum.tokensPerSecond = writingMs >= MINIMUM_RATE_MS ? (written / writingMs) * 1000 : 0
  return sum
}

// contextUse is how much of the context window the conversation fills after the last reported call:
// its input and its output, which the next call reads again.
export function contextUse(calls: Usage[]): { used: number; window: number; share: number } | null {
  const last = calls.at(-1)
  if (!last) return null
  const used = last.inputTokens + last.outputTokens
  return { used, window: last.contextLength, share: last.contextLength ? used / last.contextLength : 0 }
}

// ServerPrompt is the prompt processing the model server measured over calls: the tokens it read
// (cached ones not counted), the time and the rate. null when no call carries the server's timings.
export type ServerPrompt = { ms: number; tokens: number; tokensPerSecond: number }

export function serverPrompt(calls: Usage[]): ServerPrompt | null {
  let ms = 0
  let tokens = 0
  for (const call of calls) {
    if (!call.promptMs) continue
    ms += call.promptMs
    tokens += call.promptTokens ?? 0
  }
  return ms > 0 ? { ms, tokens, tokensPerSecond: (tokens / ms) * 1000 } : null
}

// generationRate is the server's own generation rate over its calls, their output weighted, or
// the rate of the reported times where the server sent none.
export function generationRate(calls: Usage[]): number {
  let tokens = 0
  let ms = 0
  for (const call of calls) {
    if (!call.predictedPerSecond || !call.outputTokens) continue
    tokens += call.outputTokens
    ms += (call.outputTokens / call.predictedPerSecond) * 1000
  }
  return ms > 0 ? (tokens / ms) * 1000 : totals(calls).tokensPerSecond
}

// draftShare is the share of speculative-decoding drafts the model server kept, null without drafts.
export function draftShare(calls: Usage[]): number | null {
  const drafted = calls.reduce((sum, call) => sum + (call.draftTokens ?? 0), 0)
  return drafted ? calls.reduce((sum, call) => sum + (call.draftAccepted ?? 0), 0) / drafted : null
}

export const seconds = (ms: number) => (ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`)
export const rate = (tokensPerSecond: number | null) => (tokensPerSecond ? `${tokensPerSecond.toFixed(1)} tok/s` : '–')
export const count = (tokens: number) => tokens.toLocaleString('en-US')
