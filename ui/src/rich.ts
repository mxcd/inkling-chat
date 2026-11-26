// The answer as the system prompt asks for it: paragraphs, "- " lists and fenced code. Every word
// of the prose gets a key in reading order, so while the answer streams only the words that just
// arrived are new and play their entrance once; a word still being written keeps its key.
export type Word = { key: number; text: string }
export type Block =
  | { kind: 'code'; key: string; code: string }
  | { kind: 'p'; key: string; words: Word[] }
  | { kind: 'ul'; key: string; items: Word[][] }

export function parseRich(text: string): Block[] {
  const blocks: Block[] = []
  let wordKey = 0
  const words = (line: string): Word[] => (line.match(/\S+\s*/g) ?? []).map(word => ({ key: wordKey++, text: word }))
  text.split('```').forEach((part, index) => {
    if (index % 2 === 1) {
      const lineEnd = part.indexOf('\n')
      const code = lineEnd < 0 ? '' : part.slice(lineEnd + 1)
      blocks.push({ kind: 'code', key: `c${index}`, code: code.replace(/\n$/, '') })
      return
    }
    let items: Word[][] = []
    const flush = () => {
      if (items.length) blocks.push({ kind: 'ul', key: `u${index}.${blocks.length}`, items })
      items = []
    }
    for (const line of part.split('\n')) {
      const item = /^\s*[-•]\s+(.*)$/.exec(line)
      if (item) {
        items.push(words(item[1]))
      } else {
        flush()
        if (line.trim()) blocks.push({ kind: 'p', key: `p${index}.${blocks.length}`, words: words(line) })
      }
    }
    flush()
  })
  return blocks
}
