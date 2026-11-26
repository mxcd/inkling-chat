<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { contextUse, count, draftShare, generationRate, liveView, rate, seconds, serverPrompt, totals, type TurnStats } from '../metrics'
import type { Usage } from '../api'

const props = defineProps<{ stats: TurnStats }>()

// the running call's numbers move with the clock, so they are read ten times a second
const now = ref(performance.now())
let timer = 0
watch(() => props.stats.live !== null, running => {
  clearInterval(timer)
  if (running) timer = window.setInterval(() => { now.value = performance.now() }, 100)
}, { immediate: true })
onBeforeUnmount(() => clearInterval(timer))

const sum = computed(() => totals(props.stats.calls))
const live = computed(() => (props.stats.live ? liveView(props.stats.live, now.value) : null))

// the server's own prompt processing when it measures it; else the time to the first token
const prompt = computed(() => serverPrompt(props.stats.calls))
const promptMs = computed(() => sum.value.latencyMs + (live.value ? (live.value.phase === 'prompt' ? live.value.elapsedMs : live.value.latencyMs) : 0))
const callPrompt = (call: Usage) => (call.promptMs ? seconds(call.promptMs) : `${seconds(call.latencyMs)} TTFT`)
const callPromptRate = (call: Usage) => (call.promptMs ? rate(call.promptPerSecond || null) : '–')
const drafts = computed(() => draftShare(props.stats.calls))
const outputTokens = computed(() => sum.value.outputTokens + (live.value?.phase === 'writing' ? live.value.tokens : 0))
const totalMs = computed(() => sum.value.durationMs + (live.value?.elapsedMs ?? 0))
const generation = computed(() => (live.value?.phase === 'writing' ? live.value.tokensPerSecond : generationRate(props.stats.calls) || null))
const cachedShare = computed(() => (sum.value.inputTokens ? Math.round((sum.value.cachedInputTokens / sum.value.inputTokens) * 100) : 0))
// the input of a call is known only once the server reports it
const pending = computed(() => props.stats.calls.length === 0)
const context = computed(() => contextUse(props.stats.calls))
const showCalls = computed(() => props.stats.calls.length + (live.value ? 1 : 0) > 1)
</script>

<template>
  <section class="expert" aria-label="Expert details">
    <dl class="expert-grid">
      <div :class="{ live: live?.phase === 'prompt' }">
        <dt>Prompt processing</dt>
        <template v-if="prompt">
          <dd>{{ rate(prompt.tokensPerSecond) }}</dd>
          <dd class="expert-sub">{{ seconds(prompt.ms) }} · {{ count(prompt.tokens) }} tok</dd>
        </template>
        <template v-else>
          <dd>{{ live?.phase === 'prompt' ? '…' : '–' }}</dd>
          <dd class="expert-sub">{{ seconds(promptMs) }} to first token</dd>
        </template>
      </div>
      <div>
        <dt>Input tokens</dt>
        <dd>{{ pending ? '…' : count(sum.inputTokens) }}</dd>
      </div>
      <div>
        <dt>Cached input</dt>
        <dd v-if="pending">…</dd>
        <dd v-else>{{ count(sum.cachedInputTokens) }} <small>{{ cachedShare }} %</small></dd>
      </div>
      <div :class="{ live: live?.phase === 'writing' }">
        <dt>Output tokens</dt>
        <dd>{{ count(outputTokens) }}</dd>
      </div>
      <div :class="{ live: live?.phase === 'writing' }">
        <dt>Generation</dt>
        <dd>{{ rate(generation) }}<small v-if="drafts !== null && live?.phase !== 'writing'" title="Share of the drafted tokens the model kept (speculative decoding)">draft {{ Math.round(drafts * 100) }} %</small></dd>
      </div>
      <div :class="{ live: !!live }">
        <dt>Total time</dt>
        <dd>{{ seconds(totalMs) }}</dd>
      </div>
    </dl>
    <div class="expert-context">
      <span class="expert-context-label">Context window</span>
      <template v-if="context">
        <span class="expert-context-bar" role="meter" :aria-valuenow="context.used" aria-valuemin="0" :aria-valuemax="context.window || undefined" aria-label="Context window used">
          <span :style="{ width: `${Math.min(100, context.share * 100)}%` }" />
        </span>
        <span class="expert-context-value">
          {{ count(context.used) }}<template v-if="context.window"> / {{ count(context.window) }} <small>{{ (context.share * 100).toFixed(1) }} %</small></template>
        </span>
      </template>
      <span v-else class="expert-context-value">…</span>
    </div>
    <table v-if="showCalls" class="expert-calls">
      <thead>
        <tr><th>Call</th><th>PP rate</th><th>PP time</th><th>Input</th><th>Cached</th><th>Output</th><th>Rate</th><th>Time</th></tr>
      </thead>
      <tbody>
        <tr v-for="(call, index) in stats.calls" :key="index">
          <td>{{ index + 1 }}</td>
          <td>{{ callPromptRate(call) }}</td>
          <td :title="call.promptMs ? `${count(call.promptTokens ?? 0)} tokens read by the model server` : 'Time to the first token'">{{ callPrompt(call) }}</td>
          <td>{{ count(call.inputTokens) }}</td>
          <td>{{ count(call.cachedInputTokens) }}</td>
          <td>{{ count(call.outputTokens) }}</td>
          <td>{{ rate(call.predictedPerSecond || call.tokensPerSecond || null) }}</td>
          <td>{{ seconds(call.durationMs) }}</td>
        </tr>
        <tr v-if="live" class="live">
          <td>{{ stats.calls.length + 1 }}</td>
          <td>…</td>
          <td>{{ seconds(live.phase === 'prompt' ? live.elapsedMs : live.latencyMs) }}</td>
          <td>…</td>
          <td>…</td>
          <td>{{ live.phase === 'writing' ? `~${count(live.tokens)}` : '…' }}</td>
          <td>{{ live.phase === 'writing' ? rate(live.tokensPerSecond) : '…' }}</td>
          <td>{{ seconds(live.elapsedMs) }}</td>
        </tr>
      </tbody>
    </table>
    <p v-if="live" class="expert-note">
      {{ live.phase === 'prompt' ? 'Processing the prompt…' : 'Writing: counted in this browser until the server reports the call.' }}
    </p>
  </section>
</template>
