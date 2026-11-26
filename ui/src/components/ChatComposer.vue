<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { ArrowUp, Brain, ChevronDown, FolderOpen, Square, UserRound } from 'lucide-vue-next'
import type { Persona } from '../api'
import { THINKING_LABELS, THINKING_LEVELS, type Thinking } from '../thinking'
import DirectoryControl from './DirectoryControl.vue'

const props = defineProps<{ busy: boolean; personas: Persona[] }>()
const text = defineModel<string>({ required: true })
const workspace = defineModel<boolean>('workspace', { required: true })
const directory = defineModel<string>('directory', { required: true })
const thinking = defineModel<Thinking>('thinking', { required: true })
const persona = defineModel<string>('persona', { required: true })
const emit = defineEmits<{ submit: []; stop: [] }>()

const field = ref<HTMLTextAreaElement>()
const canSend = computed(() => !props.busy && text.value.trim().length > 0)
const send = () => { if (canSend.value) emit('submit') }

// grow with the text
watch(text, async () => {
  await nextTick()
  const element = field.value
  if (!element) return
  element.style.height = ''
  element.style.height = `${Math.min(element.scrollHeight, 200)}px`
})

const onKeydown = (event: KeyboardEvent) => {
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
    event.preventDefault()
    send()
  }
}

defineExpose({ focus: () => field.value?.focus() })
</script>

<template>
  <form class="composer" @submit.prevent="send">
    <textarea
      ref="field"
      v-model="text"
      rows="1"
      :placeholder="workspace ? 'Ask for something to do in the directory…' : 'Ask anything…'"
      aria-label="Your message"
      enterkeyhint="send"
      autofocus
      @keydown="onKeydown"
    />
    <div class="composer-bar">
      <button
        type="button"
        :class="['toggle', { on: workspace }]"
        :aria-pressed="workspace"
        title="With the tools the model reads and writes the files of the chat's directory and runs commands there in a virtual shell, with the host's go and bun. The answer then arrives in one piece instead of streaming."
        @click="workspace = !workspace"
      >
        <FolderOpen :size="15" />
        File tools {{ workspace ? 'on' : 'off' }}
      </button>
      <DirectoryControl v-model="directory" />
      <label
        :class="['toggle', 'toggle-select', { on: thinking !== 'off' }]"
        title="How long the model thinks before it answers. Off answers at once; the thinking shows above the answer. Changing it within a conversation makes the model read the conversation again once."
      >
        <Brain :size="15" />
        Thinking
        <select v-model="thinking" aria-label="Thinking">
          <option v-for="level in THINKING_LEVELS" :key="level" :value="level">{{ THINKING_LABELS[level] }}</option>
        </select>
        <ChevronDown :size="13" class="toggle-chevron" />
      </label>
      <label
        :class="['toggle', 'toggle-select', { on: persona !== personas[0]?.id }]"
        title="The persona is the model's system prompt. Changing it within a conversation changes the system prompt, so the model reads the whole conversation again once."
      >
        <UserRound :size="15" />
        <select v-model="persona" aria-label="Persona">
          <option v-for="choice in personas" :key="choice.id" :value="choice.id" :title="choice.description">{{ choice.label }}</option>
        </select>
        <ChevronDown :size="13" class="toggle-chevron" />
      </label>
      <span class="composer-hint"><span class="kbd-hint">Enter to send · Shift + Enter for a new line</span></span>
      <button v-if="busy" type="button" class="round send" aria-label="Stop the answer" @click="emit('stop')">
        <Square :size="13" fill="currentColor" />
      </button>
      <button v-else type="submit" class="round send" :disabled="!canSend" aria-label="Send">
        <ArrowUp :size="18" :stroke-width="2.4" />
      </button>
    </div>
  </form>
</template>
