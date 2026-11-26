import { expect, test } from 'bun:test'
import { parseFrames } from './api'

test('complete frames become events and an unfinished one waits for the next chunk', () => {
  const first = parseFrames('event: text\ndata: {"text":"Hel"}\n\nevent: tool_call\ndata: {"callId":"c1","tool":"write_file","target":"a.md"}\n\nevent: te')
  expect(first.events).toEqual([
    { kind: 'text', text: 'Hel' },
    { kind: 'tool_call', callId: 'c1', tool: 'write_file', target: 'a.md' },
  ])
  expect(first.rest).toBe('event: te')

  const second = parseFrames(first.rest + 'xt\ndata: {"text":"lo\\n\\nworld"}\n\nevent: end\ndata: {"reason":"stop"}\n\n')
  expect(second.events).toEqual([{ kind: 'text', text: 'lo\n\nworld' }, { kind: 'end', reason: 'stop' }])
  expect(second.rest).toBe('')
})
