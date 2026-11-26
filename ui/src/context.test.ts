import { expect, test } from 'bun:test'
import { blockUnit, blocks, composition, estimateTokens, inWindow } from './context'
import type { ChatMessage } from './history'
import type { Usage } from './api'

const usage = (inputTokens: number, outputTokens: number): Usage => ({
  contextLength: 32768, inputTokens, cachedInputTokens: 0, outputTokens, latencyMs: 1, durationMs: 2, tokensPerSecond: 0,
})
const user = (content: string): ChatMessage => ({ id: content, role: 'user', content })
const answer = (id: string, stats: ChatMessage['stats'], workspace = false): ChatMessage => ({ id, role: 'assistant', content: 'x', status: 'done', stats, workspace })

test('the system prompt is the first input less the estimated first message, and each turn adds its own', () => {
  const question = 'Write the notes.' // 16 characters: 8 estimated tokens
  const rows = composition([
    user(question),
    // a tool call (2100 in, 30 out), then the answer after the tool result (40 more in)
    answer('a', { calls: [usage(2100, 30), usage(2170, 50)], live: null }),
    user('Thanks.'),
    answer('b', { calls: [usage(2240, 20)], live: null, thinking: 5 }),
  ])

  expect(rows.map(row => row.label)).toEqual(['System', 'Turn 1', 'Turn 2'])
  expect(rows[0].parts).toEqual({ system: 2100 - estimateTokens(question), input: 0, thinking: 0, output: 0 })
  expect(rows[1]).toMatchObject({ parts: { system: 0, input: estimateTokens(question) + 40, thinking: 0, output: 80 }, estimated: true })
  expect(rows[2]).toMatchObject({ parts: { system: 0, input: 20, thinking: 5, output: 15 }, estimated: false })
  expect(inWindow(rows)).toBe(2240 + 20) // the last call's input and output: what the next call reads
})

test('a running call adds what it wrote so far, and a turn the server never counted is left out', () => {
  const rows = composition([
    user('Hello?'),
    { id: 'failed', role: 'assistant', content: '', status: 'error', stats: { calls: [], live: null } },
    user('Hello again.'),
    answer('running', { calls: [], live: { startedAt: 0, firstTokenAt: 1, lastTokenAt: 2, pieces: 12, thinking: 4 } }),
  ])
  expect(rows).toHaveLength(1)
  expect(rows[0]).toMatchObject({ label: 'Turn 2', live: true, estimated: true, parts: { input: estimateTokens('Hello again.'), thinking: 4, output: 8 } })
})

test('any part that is there takes at least one block', () => {
  expect(blocks(0, 64)).toBe(0)
  expect(blocks(1, 64)).toBe(1)
  expect(blocks(129, 64)).toBe(3)
})

test('switching the file tools off swaps the system prompt, and old thinking leaves the total', () => {
  const rows = composition([
    user('List the files.'), // 15 characters: 8 estimated tokens
    answer('tools', { calls: [usage(2108, 24), usage(2189, 44)], live: null, thinking: 10 }, true),
    user('Write a story.'), // 14 characters: 8 estimated tokens
    answer('plain', { calls: [usage(203, 322)], live: null }),
  ])
  const turn1 = 8 + (2189 - 2132) + (24 + 44 - 10) // estimate, tool result, output without its thinking
  expect(rows.map(row => row.label)).toEqual(['System', 'Turn 1', 'Turn 2'])
  expect(rows[0].parts.system).toBe(203 - turn1 - 8)
  expect(rows[1].parts).toEqual({ system: 0, input: 8 + 57, thinking: 10, output: 58 })
  expect(rows[2]).toMatchObject({ estimated: true, parts: { input: 8, output: 322 } })
  expect(inWindow(rows)).toBe(203 + 322)
})

test('a block stands for more tokens as the context fills, so the blocks stay few', () => {
  expect(blockUnit(0)).toBe(64)
  expect(blockUnit(32_768)).toBe(64)
  expect(blockUnit(32_769)).toBe(128)
  expect(blockUnit(1_048_576)).toBe(2048)
})
