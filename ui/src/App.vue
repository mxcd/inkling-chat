<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Braces, Code, Cpu, FilePlus, FolderSearch, Gauge, Info, Lightbulb, Moon, PanelLeft, Regex, SquarePen, Sun } from 'lucide-vue-next'
import { ChatError, listPersonas, streamChat, type Persona } from './api'
import { DEFAULT_PERSONA, loadDirectory, loadPersona, saveDirectory, savePersona } from './choices'
import { loadConversations, saveConversations, titleOf, type ChatMessage, type Conversation, type ToolStep } from './history'
import { TurnMeter } from './metrics'
import { loadThinking, saveThinking, withReasoning, type Thinking } from './thinking'
import { theme } from './theme'
import { ORB } from './orb'
import AssistantMessage from './components/AssistantMessage.vue'
import ChatComposer from './components/ChatComposer.vue'
import ChatSidebar from './components/ChatSidebar.vue'
import ContextPanel from './components/ContextPanel.vue'
import { ThinkingOrb } from 'vue-agent-fx/orbs'

const SUGGESTIONS = [
  { icon: Code, area: 'Code', workspace: false, q: 'Write a Go HTTP server with graceful shutdown and a /healthz route.' },
  { icon: Lightbulb, area: 'Explain', workspace: false, q: 'Explain in five sentences how a heat pump works.' },
  { icon: FilePlus, area: 'Files', workspace: true, q: 'Create notes/ideas.md with five project ideas for a weekend.' },
  { icon: FolderSearch, area: 'Files', workspace: true, q: 'List the files in the workspace and say in one line each what they contain.' },
  { icon: Regex, area: 'Code', workspace: false, q: 'Give me a regular expression for German postal codes and explain it.' },
  { icon: Braces, area: 'Data', workspace: false, q: 'Return a JSON array of ten fictional Kubernetes pods with name, namespace and status.' },
]

const EXPERT_KEY = 'inkling-chat.expert'

const greeting = () => {
  const hour = new Date().getHours()
  return hour < 11 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening'
}

const conversations = reactive<Conversation[]>(loadConversations())
// the server keeps a conversation under its id; a new chat is a new id, kept once it has a message
const activeId = ref<string>(crypto.randomUUID())
const active = computed(() => conversations.find(conversation => conversation.id === activeId.value))
const messages = computed(() => active.value?.messages ?? [])
const empty = computed(() => messages.value.length === 0)

const input = ref('')
const workspace = ref(false)
const thinking = ref<Thinking>(loadThinking())
const directory = ref(loadDirectory())
const persona = ref(loadPersona())
// the server's list replaces this one when it arrives
const personas = ref<Persona[]>([{ id: DEFAULT_PERSONA, label: 'Assistant', description: 'A helpful general assistant.' }])
const busy = ref(false)
const expert = ref(localStorage.getItem(EXPERT_KEY) === 'true')
const sidebarOpen = ref(false)
const composer = ref<InstanceType<typeof ChatComposer>>()
let abort: AbortController | null = null

watch(expert, on => localStorage.setItem(EXPERT_KEY, String(on)))
watch(thinking, level => saveThinking(level))
// a conversation keeps its directory and persona; opening one shows them, a new chat the last ones sent
watch(activeId, () => {
  directory.value = active.value ? active.value.directory ?? '' : loadDirectory()
  persona.value = active.value ? active.value.persona ?? DEFAULT_PERSONA : loadPersona()
})
watch([directory, persona], ([chosenDirectory, chosenPersona]) => {
  const conversation = active.value
  if (!conversation || (conversation.directory === chosenDirectory && conversation.persona === chosenPersona)) return
  Object.assign(conversation, { directory: chosenDirectory, persona: chosenPersona })
  save()
})
onMounted(() => listPersonas().then(listed => { personas.value = listed }).catch(() => { /* the default persona stays choosable */ }))

const save = () => saveConversations(conversations)

// touch moves a conversation to the top of the history.
function touch(conversation: Conversation) {
  conversation.updatedAt = Date.now()
  conversations.sort((a, b) => b.updatedAt - a.updatedAt)
}

// withStep adds a tool call to an answer's steps, or settles the step its result belongs to.
function withStep(steps: ToolStep[] = [], event: { kind: 'tool_call' | 'tool_result'; callId: string; tool: string; target?: string; ok?: boolean; error?: string }): ToolStep[] {
  if (event.kind === 'tool_call') return [...steps, { id: event.callId, tool: event.tool, target: event.target, status: 'running' }]
  return steps.map(step => (step.id === event.callId ? { ...step, status: event.ok ? 'ok' : 'failed', error: event.error } : step))
}

async function ask(raw: string, withWorkspace: boolean, withThinking: Thinking = thinking.value) {
  const text = raw.trim()
  if (!text || busy.value) return
  let conversation = active.value
  if (!conversation) {
    conversations.unshift({ id: activeId.value, title: titleOf(text), updatedAt: Date.now(), messages: [], directory: directory.value, persona: persona.value })
    conversation = conversations[0]
  }
  const { directory: withDirectory = '', persona: withPersona = DEFAULT_PERSONA } = conversation
  saveDirectory(withDirectory)
  savePersona(withPersona)
  conversation.messages.push(
    { id: crypto.randomUUID(), role: 'user', content: text },
    { id: crypto.randomUUID(), role: 'assistant', content: '', status: 'streaming', workspace: withWorkspace, thinking: withThinking, stats: { calls: [], live: null } },
  )
  // the reactive copy, so every change below renders, also after a switch to another conversation
  const answer = conversation.messages[conversation.messages.length - 1] as ChatMessage & { stats: NonNullable<ChatMessage['stats']> }
  touch(conversation)
  save()
  input.value = ''
  busy.value = true
  const controller = new AbortController()
  abort = controller
  const meter = new TurnMeter(answer.stats, performance.now())
  let callEnded = false // the next call's thinking starts a paragraph of its own
  try {
    let capped = false // the turn used up its model calls
    await streamChat(conversation.id, { text, workspace: withWorkspace, directory: withDirectory, thinking: withThinking, persona: withPersona }, event => {
      switch (event.kind) {
        case 'text':
          answer.content += event.text
          meter.piece(performance.now())
          break
        case 'thinking':
          answer.reasoning = withReasoning(answer.reasoning, event.text, callEnded)
          callEnded = false
          meter.piece(performance.now(), true)
          break
        case 'usage':
          meter.usage(event.usage)
          callEnded = true
          break
        case 'tool_call':
          answer.steps = withStep(answer.steps, event)
          break
        case 'tool_result':
          answer.steps = withStep(answer.steps, event)
          meter.toolResult(performance.now())
          break
        case 'end':
          capped = event.reason === 'max_turns'
          break
      }
    }, controller.signal)
    if (answer.content) answer.status = 'done'
    else if (capped) Object.assign(answer, { status: 'error', error: 'The model used all its steps for this message without an answer. Ask it to go on.' })
    else Object.assign(answer, { status: 'error', error: 'The model returned no answer.' })
  } catch (error) {
    if (controller.signal.aborted) Object.assign(answer, answer.content ? { status: 'stopped' } : { status: 'error', error: 'Stopped.' })
    else Object.assign(answer, { status: 'error', error: error instanceof ChatError ? error.message : 'The connection failed.' })
  } finally {
    meter.finish()
    busy.value = false
    abort = null
    touch(conversation)
    save()
  }
}

// retry asks the question of a failed answer again, in place of the two.
function retry(answerIndex: number) {
  const conversation = active.value
  const question = conversation?.messages[answerIndex - 1]
  if (!conversation || question?.role !== 'user') return
  const { workspace: withWorkspace = false, thinking: withThinking = 'off' } = conversation.messages[answerIndex]
  conversation.messages.splice(answerIndex - 1, 2)
  ask(question.content, withWorkspace, withThinking)
}

function newChat() {
  activeId.value = crypto.randomUUID()
  input.value = ''
  sidebarOpen.value = false
  nextTick(() => composer.value?.focus())
}

function select(id: string) {
  activeId.value = id
  sidebarOpen.value = false
}

function remove(id: string) {
  const index = conversations.findIndex(conversation => conversation.id === id)
  if (index < 0) return
  if (busy.value && id === activeId.value) abort?.abort()
  conversations.splice(index, 1)
  save()
  if (id === activeId.value) newChat()
}

// follow the answer while it is written, unless the reader scrolled up
const main = ref<HTMLElement>()
const column = ref<HTMLDivElement>()
let pinned = true
let resized: ResizeObserver | undefined
const onScroll = () => {
  const scroller = main.value!
  pinned = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 96
}
onMounted(() => {
  const scroller = main.value!
  resized = new ResizeObserver(() => { if (pinned && scroller.querySelector('.msg-ai')) scroller.scrollTop = scroller.scrollHeight })
  resized.observe(column.value!)
})
onBeforeUnmount(() => resized?.disconnect())
watch(() => [activeId.value, messages.value.length], () => { pinned = true })
</script>

<template>
  <div class="app">
    <ChatSidebar :conversations="conversations" :active-id="activeId" :open="sidebarOpen" @select="select" @remove="remove" @create="newChat" @close="sidebarOpen = false" />
    <div class="pane">
      <header class="top">
        <div class="top-in">
          <button class="icon side-toggle" aria-label="Conversations" @click="sidebarOpen = true"><PanelLeft :size="18" /></button>
          <button class="brand" aria-label="Inkling Chat, new conversation" @click="newChat">
            <ThinkingOrb theme="auto" :size="20" :color="ORB" :state="busy ? 'composing' : 'breathing'" />
            Inkling Chat
          </button>
          <div class="tenant" title="The model runs on this computer">
            <Cpu :size="15" />
            <span>Inkling 2 Mini</span>
            <span class="tenant-sep" />
            <span class="tenant-plan">local</span>
          </div>
          <nav class="top-actions">
            <button class="ghost" :aria-label="theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'" :title="theme === 'dark' ? 'Light mode' : 'Dark mode'" @click="theme = theme === 'dark' ? 'light' : 'dark'">
              <Sun v-if="theme === 'dark'" :size="17" /><Moon v-else :size="17" />
            </button>
            <button :class="['ghost', { pressed: expert }]" :aria-pressed="expert" title="Show token counts and timings of every answer" @click="expert = !expert">
              <Gauge :size="17" /><span>Expert details</span>
            </button>
            <button class="ghost" :disabled="empty" @click="newChat"><SquarePen :size="17" /><span>New chat</span></button>
          </nav>
        </div>
      </header>

      <main ref="main" class="main" @scroll.passive="onScroll">
        <div ref="column" class="col">
          <section v-if="empty" class="welcome">
            <ThinkingOrb theme="auto" :size="64" :color="ORB" state="breathing" class="welcome-orb" :dot-size="1.3" />
            <h1>{{ greeting() }}. What shall we work on?</h1>
            <p>
              A chat agent on the model that runs on this computer. Ask it anything, or switch the file tools on and let it
              read and write the files of the workspace folder.
            </p>
            <ul class="suggest">
              <li v-for="suggestion in SUGGESTIONS" :key="suggestion.q">
                <button @click="workspace = suggestion.workspace; ask(suggestion.q, suggestion.workspace)">
                  <span class="suggest-icon"><component :is="suggestion.icon" :size="18" /></span>
                  <span><small>{{ suggestion.area }}{{ suggestion.workspace ? ' · file tools' : '' }}</small>{{ suggestion.q }}</span>
                </button>
              </li>
            </ul>
          </section>
          <template v-else>
            <template v-for="(message, index) in messages" :key="message.id">
              <div v-if="message.role === 'user'" class="msg-user">{{ message.content }}</div>
              <AssistantMessage v-else :message="message" :expert="expert" :retryable="message.status === 'error' && !busy" @retry="retry(index)" />
            </template>
          </template>
        </div>
      </main>

      <div class="dock">
        <div class="col">
          <ChatComposer ref="composer" v-model="input" v-model:workspace="workspace" v-model:directory="directory" v-model:thinking="thinking" v-model:persona="persona" :personas="personas" :busy="busy" @submit="ask(input, workspace)" @stop="abort?.abort()" />
          <p class="disclaimer">
            <Info :size="13" />
            Answers come from an AI model and can be wrong. The model runs on this computer; nothing you write leaves it.
          </p>
        </div>
      </div>
    </div>
    <ContextPanel v-if="expert" :messages="messages" />
  </div>
</template>
