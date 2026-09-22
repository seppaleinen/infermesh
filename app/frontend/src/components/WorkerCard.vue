<script setup lang="ts">
import { computed } from 'vue'

export interface WorkerCardProps {
  id: string
  address: string
  hostname: string
  status: string
  version: string
  loadedModels: string[] | null
  lastSeenRel: string
  lastSeenAbsolute: string
}

const props = defineProps<WorkerCardProps>()

// Status dot colour: green = available, amber = busy, grey = unassigned/unavailable.
const dotClass = computed(() => {
  switch (props.status) {
    case 'available':
      return 'dot-available'
    case 'busy':
      return 'dot-busy'
    default:
      return 'dot-idle'
  }
})

const statusLabel = computed(() => props.status || 'unknown')

const models = computed(() => props.loadedModels ?? [])
const hasModels = computed(() => models.value.length > 0)
</script>

<template>
  <article class="worker-card">
    <div class="worker-head">
      <span class="status-dot" :class="dotClass" :title="statusLabel" aria-hidden="true"></span>
      <div class="worker-title">
        <span class="worker-id" :title="id">{{ id }}</span>
        <span class="worker-addr">{{ address }}</span>
      </div>
      <span class="status-pill" :class="dotClass">{{ statusLabel }}</span>
    </div>

    <div class="worker-body">
      <div class="worker-row">
        <span class="worker-k">hostname</span>
        <span class="worker-v">{{ hostname || '—' }}</span>
      </div>
      <div class="worker-row">
        <span class="worker-k">version</span>
        <span class="worker-v worker-mono">{{ version || '—' }}</span>
      </div>
      <div class="worker-row">
        <span class="worker-k">loaded models</span>
        <span class="worker-v">
          <template v-if="hasModels">
            <span v-for="m in models" :key="m" class="model-chip">{{ m }}</span>
          </template>
          <template v-else>—</template>
        </span>
      </div>
      <div class="worker-row">
        <span class="worker-k">last seen</span>
        <span class="worker-v">
          <span class="worker-mono">{{ lastSeenRel }}</span>
          <span class="worker-faint">{{ lastSeenAbsolute }}</span>
        </span>
      </div>
    </div>
  </article>
</template>

<style scoped>
.worker-card {
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface);
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 14px;
  min-width: 0;
}

.worker-head {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.status-dot {
  flex: 0 0 auto;
  width: 11px;
  height: 11px;
  border-radius: 50%;
}

.dot-available {
  background: var(--accent-strong);
  box-shadow: 0 0 8px rgba(93, 252, 190, 0.45);
}

.dot-busy {
  background: #f0b429;
  box-shadow: 0 0 8px rgba(240, 180, 41, 0.45);
}

.dot-idle {
  background: var(--text-faint);
}

.worker-title {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}

.worker-id {
  font-family: var(--font-mono);
  font-size: 12px;
  letter-spacing: 0.03em;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.worker-addr {
  font-size: 11.5px;
  color: var(--text-muted);
  font-family: var(--font-mono);
}

.status-pill {
  flex: 0 0 auto;
  padding: 3px 9px;
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  border-radius: 999px;
  color: var(--surface);
}

.status-pill.dot-available {
  background: var(--accent-strong);
}

.status-pill.dot-busy {
  background: #f0b429;
  color: #1a1406;
}

.status-pill.dot-idle {
  background: var(--text-faint);
}

.worker-body {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.worker-row {
  display: flex;
  align-items: baseline;
  gap: 12px;
  font-size: 12px;
  min-width: 0;
}

.worker-k {
  flex: 0 0 96px;
  color: var(--text-faint);
  letter-spacing: 0.04em;
  text-transform: uppercase;
  font-size: 10.5px;
}

.worker-v {
  flex: 1 1 auto;
  min-width: 0;
  color: var(--text);
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.worker-mono {
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
}

.worker-faint {
  color: var(--text-faint);
  font-size: 11px;
}

.model-chip {
  padding: 2px 8px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--accent-strong);
  border: 1px solid rgba(93, 252, 190, 0.28);
  border-radius: 999px;
  background: var(--accent-dim);
}
</style>