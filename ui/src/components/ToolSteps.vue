<script setup lang="ts">
import { CircleAlert, FileText, LoaderCircle, SquareTerminal } from 'lucide-vue-next'
import type { ToolStep } from '../history'

defineProps<{ steps: ToolStep[] }>()
</script>

<template>
  <ul class="steps">
    <li v-for="step in steps" :key="step.id" :class="['step', `step-${step.status}`]" :title="step.error">
      <LoaderCircle v-if="step.status === 'running'" :size="14" class="spin" />
      <SquareTerminal v-else-if="step.status === 'ok' && (step.tool === 'bash' || step.tool.endsWith('_process') || step.tool === 'http_request')" :size="14" />
      <FileText v-else-if="step.status === 'ok'" :size="14" />
      <CircleAlert v-else :size="14" />
      <span class="step-tool">{{ step.tool }}</span>
      <span v-if="step.target" class="step-target" :title="step.target">{{ step.target }}</span>
    </li>
  </ul>
</template>
