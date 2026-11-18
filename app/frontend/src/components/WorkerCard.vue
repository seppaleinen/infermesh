<script setup lang="ts">
import { computed } from 'vue'
import type { ModelView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'

export interface WorkerCardProps {
  id: string
  address: string
  hostname: string
  status: string
  version: string
  loadedModels: string[] | null
  advertisedModels?: ModelView[] | null
  lastSeenRel: string
  lastSeenAbsolute: string
  isLocal?: boolean
  inFlight?: number
  maxInFlight?: number
  tokensPerSecond?: number
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

// The "available models" row renders the worker's advertised capability
// list when the router surfaces it (issue #205). Loaded models render as
// filled accent chips; advertised-but-not-loaded models render as
// outlined chips so LM Studio workers (which report Loaded=false for
// every model) still show their models instead of a "—". When the router
// provides no advertised list we fall back to the legacy LoadedModels
// string list, and when both are empty we show "—".
const advertised = computed(() => props.advertisedModels ?? [])
const hasAdvertised = computed(() => advertised.value.length > 0)

interface ModelChip {
  name: string
  loaded: boolean
  concurrencyCap?: number
}

const chips = computed((): ModelChip[] => {
  if (hasAdvertised.value) {
    return advertised.value.map(m => ({
      name: m.name,
      loaded: m.loaded,
      concurrencyCap: m.concurrency_cap || undefined,
    }))
  }
  return (props.loadedModels ?? []).map(name => ({ name, loaded: true }))
})

const hasModels = computed(() => chips.value.length > 0)

// Concurrency ratio ("3/5 calls"). Hidden when the router reports no cap
// (maxInFlight <= 0) so idle workers don't show a meaningless "0/0".
const busyRatio = computed(() => {
  if (props.inFlight !== undefined && props.maxInFlight !== undefined && props.maxInFlight > 0) {
    return `${props.inFlight}/${props.maxInFlight} calls`
  }
  return null
})

// Throughput badge: hidden when the router reports no throughput data
// (tokensPerSecond <= 0 or undefined), so idle workers don't show a
// meaningless "0.00 tok/s".
const throughput = computed(() => {
  const tps = props.tokensPerSecond
  if (tps === undefined || tps === null || tps <= 0) return null
  if (tps >= 100) return `${tps.toFixed(0)} tok/s`
  if (tps >= 10) return `${tps.toFixed(1)} tok/s`
  return `${tps.toFixed(2)} tok/s`
})
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
      <span v-if="isLocal" class="local-badge">You</span>
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
        <span class="worker-k">available models</span>
        <span class="worker-v">
          <template v-if="hasModels">
            <span
              v-for="m in chips"
              :key="m.name"
              class="model-chip"
              :class="{ 'model-chip-advertised': !m.loaded }"
              :title="m.loaded ? (m.concurrencyCap ? `loaded in VRAM • concurrency: ${m.concurrencyCap}` : 'loaded in VRAM') : (m.concurrencyCap ? `advertised, not yet loaded • concurrency: ${m.concurrencyCap}` : 'advertised, not yet loaded')"
            >
              {{ m.name }}
              <span v-if="m.concurrencyCap !== undefined && m.concurrencyCap > 0" class="model-chip-concurrency">{{ m.concurrencyCap }}</span>
            </span>
          </template>
          <template v-else>—</template>
        </span>
      </div>
      <div v-if="busyRatio" class="worker-row">
        <span class="worker-k">concurrency</span>
        <span class="worker-v">
          <span class="worker-mono">{{ busyRatio }}</span>
        </span>
      </div>
      <div v-if="throughput" class="worker-row">
        <span class="worker-k">throughput</span>
        <span class="worker-v">
          <span class="worker-mono throughput-badge">{{ throughput }}</span>
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

.local-badge {
  flex: 0 0 auto;
  padding: 2px 8px;
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--accent-strong);
  border: 1px solid rgba(62, 207, 174, 0.28);
  border-radius: 999px;
  background: var(--accent-dim);
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

.throughput-badge {
  color: var(--accent-strong);
  border: 1px solid rgba(93, 252, 190, 0.28);
  border-radius: 999px;
  background: var(--accent-dim);
  padding: 1px 8px;
  font-size: 10.5px;
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
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.model-chip-advertised {
  color: var(--text-muted);
  border-color: var(--border);
  background: transparent;
}

.model-chip-concurrency {
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  color: var(--surface);
  background: var(--accent-strong);
  border-radius: 999px;
  padding: 0 5px;
  line-height: 1.2;
}

.model-chip-advertised .model-chip-concurrency {
  color: var(--text);
  background: var(--text-muted);
}
</style>