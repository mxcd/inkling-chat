import { expect, test } from 'bun:test'
import { contextUse, draftShare, generationRate, liveView, serverPrompt, totals, TurnMeter, type TurnStats } from './metrics'

const call = { contextLength: 1000, inputTokens: 100, cachedInputTokens: 80, outputTokens: 50, latencyMs: 200, durationMs: 1200, tokensPerSecond: 50 }

test('a call processes its prompt until the first piece, then writes at the rate of its pieces', () => {
  const stats: TurnStats = { calls: [], live: null }
  const meter = new TurnMeter(stats, 0)
  expect(liveView(stats.live!, 300)).toEqual({ phase: 'prompt', elapsedMs: 300 })

  for (let now = 400; now <= 900; now += 10) meter.piece(now) // 51 pieces, 50 gaps of 10 ms: 100 tokens/s
  const writing = liveView(stats.live!, 950)
  expect(writing).toMatchObject({ phase: 'writing', latencyMs: 400, tokens: 51, elapsedMs: 950 })
  expect(writing.phase === 'writing' && writing.tokensPerSecond).toBeCloseTo(100, 5)
})

test('the server usage ends a call and a tool result starts the next', () => {
  const stats: TurnStats = { calls: [], live: null }
  const meter = new TurnMeter(stats, 0)
  meter.piece(10)
  meter.usage(call)
  expect(stats.live).toBeNull()
  meter.toolResult(2000)
  expect(liveView(stats.live!, 2500)).toEqual({ phase: 'prompt', elapsedMs: 500 })
  meter.finish()
  expect(stats).toEqual({ calls: [call], live: null, thinking: 0 })
})

test('a rate under a quarter second of writing is not measured', () => {
  const stats: TurnStats = { calls: [], live: null }
  const meter = new TurnMeter(stats, 0)
  meter.piece(100)
  meter.piece(200)
  const view = liveView(stats.live!, 200)
  expect(view.phase === 'writing' && view.tokensPerSecond).toBeNull()
})

test('the totals rate is all output over all writing time, without calls too short to measure', () => {
  const sum = totals([call, call, { ...call, outputTokens: 3, durationMs: 210, tokensPerSecond: 0 }])
  expect(sum).toMatchObject({ calls: 3, inputTokens: 300, cachedInputTokens: 240, outputTokens: 103, latencyMs: 600, durationMs: 2610 })
  expect(sum.tokensPerSecond).toBeCloseTo(50, 5) // 100 tokens over 2 s of writing
  expect(totals([]).tokensPerSecond).toBe(0)
})

test('the context in use is the input and output of the last call and output against its window', () => {
  expect(contextUse([])).toBeNull()
  expect(contextUse([call, { ...call, inputTokens: 300, outputTokens: 100 }])).toEqual({ used: 400, window: 1000, share: 0.4 })
  expect(contextUse([{ ...call, contextLength: 0 }])).toEqual({ used: 150, window: 0, share: 0 })
})

test('the server prompt processing sums the measured calls, and is null without them', () => {
  expect(serverPrompt([call])).toBeNull()
  const measured = { ...call, promptMs: 1000, promptTokens: 2000, predictedPerSecond: 100, draftTokens: 10, draftAccepted: 4 }
  expect(serverPrompt([measured, call, { ...measured, promptMs: 500, promptTokens: 1000 }])).toEqual({ ms: 1500, tokens: 3000, tokensPerSecond: 2000 })
  expect(draftShare([measured, call])).toBeCloseTo(0.4, 5)
  expect(draftShare([call])).toBeNull()
})

test('the generation rate is the server rate weighted by output, else the reported times', () => {
  const fast = { ...call, outputTokens: 100, predictedPerSecond: 100 } // 1 s
  const slow = { ...call, outputTokens: 50, predictedPerSecond: 25 } // 2 s
  expect(generationRate([fast, slow])).toBeCloseTo(50, 5)
  expect(generationRate([call])).toBeCloseTo(totals([call]).tokensPerSecond, 5)
})
