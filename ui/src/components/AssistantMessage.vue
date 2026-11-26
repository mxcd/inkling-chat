<script setup lang="ts">
import { computed, ref, toRef } from 'vue'
import { Check, Copy, RotateCcw } from 'lucide-vue-next'
import type { ChatMessage } from '../history'
import { generationRate, totals } from '../metrics'
import { ORB } from '../orb'
import { useSmoothText } from '../smoothText'
import ExpertDetails from './ExpertDetails.vue'
import RichText from './RichText.vue'
import { ThinkingOrb } from 'vue-agent-fx/orbs'
import ToolSteps from './ToolSteps.vue'

const props = defineProps<{ message: ChatMessage; expert: boolean; retryable: boolean }>()
const emit = defineEmits<{ retry: [] }>()

const streaming = computed(() => props.message.status === 'streaming')
const shown = useSmoothText(toRef(() => props.message.content), streaming)
const settled = computed(() => !streaming.value && shown.value.length === props.message.content.length)
const generating = computed(() => !settled.value && props.message.status !== 'error')
const waiting = computed(() => streaming.value && !shown.value && !props.message.steps?.some(step => step.status === 'running'))
const summary = computed(() => {
  const calls = props.message.stats?.calls ?? []
  return calls.length ? { outputTokens: totals(calls).outputTokens, tokensPerSecond: generationRate(calls) } : null
})

const copied = ref(false)
const copy = async () => {
  await navigator.clipboard.writeText(props.message.content)
  copied.value = true
  setTimeout(() => { copied.value = false }, 1600)
}
</script>

<template>
  <article class="msg-ai" :aria-busy="streaming">
    <div class="msg-ai-head">
      <!-- one orb for the whole turn: composing from the question until the last word is written -->
      <ThinkingOrb theme="auto" :size="32" :color="ORB" :dot-size="1.2" :state="generating ? 'composing' : 'breathing'" :paused="!generating" />
      <span class="msg-ai-name">Inkling</span>
      <span v-if="waiting" class="thinking">{{ message.workspace ? 'Thinking about the files…' : 'Thinking…' }}</span>
    </div>
    <details v-if="message.reasoning" class="reasoning">
      <summary>Thinking</summary>
      <div class="reasoning-text">{{ message.reasoning.trim() }}</div>
    </details>
    <ToolSteps v-if="message.steps?.length" :steps="message.steps" />
    <RichText v-if="shown" :text="shown" :animate="!settled" />
    <div v-if="message.status === 'error'" class="msg-error">
      {{ message.error }}
      <button v-if="retryable" class="link" @click="emit('retry')"><RotateCcw :size="14" /> Try again</button>
    </div>
    <ExpertDetails v-if="expert && message.stats" :stats="message.stats" />
    <div v-if="settled && message.status !== 'error' && message.content" class="msg-actions">
      <button class="ghost" aria-label="Copy the answer" @click="copy">
        <Check v-if="copied" :size="15" /><Copy v-else :size="15" />
        {{ copied ? 'Copied' : 'Copy' }}
      </button>
      <span v-if="!expert && summary" class="stats" title="As the model server reported it">
        {{ summary.outputTokens }} tokens<template v-if="summary.tokensPerSecond"> · {{ Math.round(summary.tokensPerSecond) }} tokens/s</template>
        <template v-if="message.status === 'stopped'"> · stopped</template>
      </span>
    </div>
  </article>
</template>
