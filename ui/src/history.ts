import type { TurnStats } from './metrics'
import type { Thinking } from './thinking'

export type ToolStep = { id: string; tool: string; target?: string; status: 'running' | 'ok' | 'failed'; error?: string }

export type ChatMessage = {
  id: string
  role: 'user' | 'assistant'
  content: string
  status?: 'streaming' | 'done' | 'stopped' | 'error'
  error?: string
  workspace?: boolean
  // the thinking the question asked for, and the reasoning the model streamed before it answered
  thinking?: Thinking
  reasoning?: string
  steps?: ToolStep[]
  stats?: TurnStats
}

// Conversation is one chat as this browser keeps it. Its id is the server's conversation id, so
// a conversation opened again continues where it was, as long as the server still remembers it
// (it keeps conversations in memory until it restarts). directory is where its tools work (empty:
// the server's workspace) and persona its system prompt; both are sent with every message, and a
// conversation from before they existed has neither.
export type Conversation = { id: string; title: string; updatedAt: number; messages: ChatMessage[]; directory?: string; persona?: string }

const KEY = 'inkling-chat.conversations'
const TITLE_LENGTH = 60

export const titleOf = (text: string) => {
  const line = text.trim().split('\n')[0]
  return line.length > TITLE_LENGTH ? `${line.slice(0, TITLE_LENGTH - 1)}…` : line
}

// loadConversations reads the kept conversations, newest first. An answer that was still
// streaming when the page closed is stopped: its stream is gone.
export function loadConversations(storage: Storage = localStorage): Conversation[] {
  let kept: Conversation[]
  try {
    kept = JSON.parse(storage.getItem(KEY) ?? '[]') as Conversation[]
  } catch {
    return [] // a damaged entry must not keep the app from starting
  }
  if (!Array.isArray(kept)) return []
  for (const conversation of kept) {
    for (const message of conversation.messages) {
      if (message.status === 'streaming') message.status = message.content ? 'stopped' : 'error'
      if (message.status === 'error') message.error ??= 'Stopped.'
      if (message.stats) message.stats.live = null
      message.steps = message.steps?.map(step => (step.status === 'running' ? { ...step, status: 'failed' } : step))
    }
  }
  return kept.sort((a, b) => b.updatedAt - a.updatedAt)
}

// saveConversations keeps the conversations that have messages. A full storage keeps the last
// state that fit instead of failing the chat.
export function saveConversations(conversations: Conversation[], storage: Storage = localStorage) {
  try {
    storage.setItem(KEY, JSON.stringify(conversations.filter(conversation => conversation.messages.length > 0)))
  } catch {
    // shortcut: quota errors drop the save silently; prune old conversations if that ever matters
  }
}
