import { expect, test } from 'bun:test'
import { parseRich } from './rich'

test('prose, lists and fenced code become blocks with words keyed in reading order', () => {
  const blocks = parseRich('Hello world\n- one\n- two\n```go\nfmt.Println()\n```\nBye')
  expect(blocks.map(block => block.kind)).toEqual(['p', 'ul', 'code', 'p'])
  expect(blocks[0]).toMatchObject({ words: [{ key: 0, text: 'Hello ' }, { key: 1, text: 'world' }] })
  expect(blocks[1]).toMatchObject({ items: [[{ key: 2, text: 'one' }], [{ key: 3, text: 'two' }]] })
  expect(blocks[2]).toMatchObject({ code: 'fmt.Println()' })
})

test('a word still being written keeps its key, so it does not play its entrance again', () => {
  const before = parseRich('Hel')[0]
  const after = parseRich('Hello the')[0]
  expect(before.kind === 'p' && before.words[0].key).toBe(0)
  expect(after.kind === 'p' && after.words.map(word => word.key)).toEqual([0, 1])
})
