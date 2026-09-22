<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import WorkerCard from './components/WorkerCard.vue'
import SettingsForm from './components/SettingsForm.vue'
import { useWorkers, relativeLastSeen, formatAbsolute } from './composables/useWorkers'

const version = 'v0.1.0'

// Live heartbeat: the Go side emits `time` every 5s with an RFC1123 stamp
// as `{ data: string }`. Mirrors the scaffold's footer pattern.
const heartbeatText = ref('Waiting for heartbeat…')
const heartbeatAlive = ref(false)

let unsubscribe: (() => void) | undefined
let staleTimer: ReturnType<typeof setTimeout> | undefined

// Strip "Mon, 21 Sep 2026 12:34:56 UTC" down to a compact HH:MM:SS for the
// footer clock.
const clock = computed(() => {
  const match = heartbeatText.value.match(/\d{1,2}:\d{2}:\d{2}/)
  return match ? match[0] : heartbeatText.value
})

function scheduleStale() {
  if (staleTimer !== undefined) clearTimeout(staleTimer)
  staleTimer = setTimeout(() => {
    heartbeatAlive.value = false
  }, 15_000)
}

// Connected-workers view (issue #41). Bound Go service: RouterClient.
const workers = useWorkers()

// Active view: 'dashboard' (workers) or 'settings'.
const activeView = ref<'dashboard' | 'settings'>('dashboard')

function onSettingsSaved(): void {
  activeView.value = 'dashboard'
  workers.retry() // re-fetch from new router URL immediately
}

onMounted(() => {
  unsubscribe = Events.On('time', (ev: { data: string }) => {
    heartbeatText.value = ev.data
    heartbeatAlive.value = true
    scheduleStale()
  })
  workers.start()
})

onBeforeUnmount(() => {
  unsubscribe?.()
  if (staleTimer !== undefined) clearTimeout(staleTimer)
  workers.stop()
})

const workerCount = computed(() => {
  if (workers.state.value.kind === 'ready') return workers.state.value.workers.length
  return 0
})
</script>

<template>
  <div class="shell">
    <header class="header">
      <div class="brand">
        <svg class="brand-glyph" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M12 3.5 20 8v8l-8 4.5L4 16V8l8-4.5Z" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" />
          <path d="M12 3.5v8l8 4.5M12 11.5 4 16" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" opacity="0.55" />
          <circle cx="12" cy="11.5" r="1.7" fill="currentColor" />
        </svg>
        <span class="brand-name">InferMesh</span>
        <span class="brand-divider" aria-hidden="true"></span>
        <span class="brand-sub">Desktop</span>
      </div>
      <nav class="nav" role="tablist" aria-label="Main navigation">
        <button
          role="tab"
          :aria-selected="activeView === 'dashboard'"
          @click="activeView = 'dashboard'"
          :class="{ active: activeView === 'dashboard' }"
        >
          Dashboard
        </button>
        <span class="nav-divider" aria-hidden="true">|</span>
        <button
          role="tab"
          :aria-selected="activeView === 'settings'"
          @click="activeView = 'settings'"
          :class="{ active: activeView === 'settings' }"
        >
          Settings
        </button>
      </nav>
      <span class="badge">{{ version }}</span>
    </header>

    <main class="main">
      <SettingsForm
        v-if="activeView === 'settings'"
        @saved="onSettingsSaved"
        @cancel="activeView = 'dashboard'"
      />
      <template v-else>
        <!-- Loading: first GetWorkers() in flight -->
        <section v-if="workers.state.value.kind === 'loading'" class="status-card" aria-live="polite">
        <div class="status-glyph" aria-hidden="true">
          <svg class="spin" viewBox="0 0 24 24" fill="none">
            <circle cx="12" cy="12" r="9" stroke="currentColor" stroke-width="2.5" opacity="0.25" />
            <path d="M12 3a9 9 0 0 1 9 9" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" />
          </svg>
        </div>
        <h1 id="status-title" class="status-title">Connecting to router…</h1>
        <p class="status-copy">Querying <span class="mono">{{ workers.routerURL.value || 'http://127.0.0.1:8080' }}</span> for connected workers.</p>
        <div class="status-flag" role="status">
          <span class="dot flag-dot"></span>
          <span>polling</span>
        </div>
      </section>

      <!-- Router reachable, 0 workers -->
      <section v-else-if="workers.state.value.kind === 'empty'" class="status-card" aria-live="polite">
        <div class="status-glyph" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none">
            <circle cx="5" cy="12" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <circle cx="19" cy="6" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <circle cx="19" cy="18" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <path d="M7 11l9.5-3.5M7 13l9.5 3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" opacity="0.6" />
          </svg>
        </div>
        <h1 id="status-title" class="status-title">No workers connected</h1>
        <p class="status-copy">Router is reachable at <span class="mono">{{ workers.state.value.routerURL }}</span>, but no worker has registered yet. Start a worker to join the pool.</p>
        <span class="status-tag">pool dashboard — issue #41</span>
        <div class="status-flag" role="status">
          <span class="dot flag-dot flag-live"></span>
          <span>router online · 0 workers</span>
        </div>
      </section>

      <!-- Router unreachable -->
      <section v-else-if="workers.state.value.kind === 'error'" class="status-card" aria-live="assertive">
        <div class="status-glyph error-glyph" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none">
            <circle cx="12" cy="12" r="9" stroke="currentColor" stroke-width="1.5" />
            <path d="M12 8v5M12 16h.01" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
          </svg>
        </div>
        <h1 id="status-title" class="status-title">Router unreachable</h1>
        <p class="status-copy">{{ workers.state.value.message }}</p>
        <span class="status-tag error-tag">router offline</span>
        <button class="retry-btn" @click="workers.retry()">
          <svg viewBox="0 0 24 24" fill="none" width="14" height="14">
            <path d="M3 12a9 9 0 0 1 9-9 9 9 0 0 1 6.4 2.6M3 5v4h4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
            <path d="M21 12a9 9 0 0 1-9 9 9 9 0 0 1-6.4-2.6M21 19v-4h-4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
          </svg>
          Retry
        </button>
        <div class="status-flag" role="status">
          <span class="dot flag-dot flag-offline"></span>
          <span>{{ workers.state.value.routerURL }}</span>
        </div>
      </section>

      <!-- Workers present -->
      <template v-else>
        <div class="workers-head">
          <div class="workers-title">
            <h1 class="workers-count">{{ workerCount }} worker<span v-if="workerCount !== 1">s</span></h1>
            <p class="workers-sub">connected to <span class="mono">{{ workers.state.value.routerURL }}</span></p>
          </div>
          <span class="workers-live"><span class="dot flag-dot flag-live"></span> live</span>
        </div>
        <ul class="worker-list">
          <li v-for="w in workers.state.value.workers" :key="w.id" class="worker-item">
            <WorkerCard
              :id="w.id"
              :address="w.address"
              :hostname="w.hostname"
              :status="w.status"
              :version="w.version"
              :loaded-models="w.loaded_models"
              :last-seen-rel="relativeLastSeen(w.last_seen)"
              :last-seen-absolute="formatAbsolute(w.last_seen)"
            />
          </li>
        </ul>
      </template>
      </template>
    </main>

    <footer class="footer">
      <span class="footer-meta">InferMesh Desktop</span>
      <span class="footer-heartbeat" :title="heartbeatText">
        <span class="dot heartbeat-dot" :class="{ 'is-live': heartbeatAlive }"></span>
        <span class="footer-clock">{{ clock }}</span>
      </span>
      <span class="footer-meta">heartbeat · 5&nbsp;s</span>
    </footer>
  </div>
</template>

<style scoped>
.shell {
  height: 100%;
  display: flex;
  flex-direction: column;
}

/* ----- Header ------------------------------------------------------------- */
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 17px 22px;
  border-bottom: 1px solid var(--border);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
}

.brand-glyph {
  width: 22px;
  height: 22px;
  color: var(--accent-strong);
}

.brand-name {
  font-size: 15px;
  font-weight: 700;
  letter-spacing: -0.01em;
}

.brand-divider {
  width: 1px;
  height: 14px;
  margin: 0 1px;
  background: var(--border-strong);
}

.brand-sub {
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.14em;
  text-transform: uppercase;
  color: var(--text-faint);
}

.badge {
  padding: 3px 9px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  letter-spacing: 0.04em;
  color: var(--text-muted);
  border: 1px solid var(--border-strong);
  border-radius: 990px;
  background: var(--surface-2);
}

/* ----- Nav ------------------------------------------------------------------ */
.nav {
  display: flex;
  align-items: center;
  gap: 4px;
}

.nav button {
  padding: 6px 14px;
  font-size: 13px;
  font-weight: 600;
  color: var(--text-muted);
  background: transparent;
  border: none;
  border-radius: 8px;
  cursor: pointer;
  transition: color 0.15s ease, background 0.15s ease;
}

.nav button:hover {
  color: var(--text);
  background: var(--surface-2);
}

.nav button.active {
  color: var(--accent-strong);
  background: var(--accent-dim);
}

.nav-divider {
  color: var(--text-faint);
  font-size: 11px;
  margin: 0 2px;
}

/* ----- Main / states ------------------------------------------------------- */
.main {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 28px;
  min-height: 0;
}

.status-card {
  width: min(100%, 430px);
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  padding: 38px 32px 0;
  border: 1px dashed var(--border-strong);
  border-radius: 16px;
  background: var(--surface);
}

.status-glyph {
  width: 56px;
  height: 56px;
  margin-bottom: 20px;
  display: grid;
  place-items: center;
  color: var(--accent-strong);
  border: 1px solid rgba(62, 207, 174, 0.22);
  border-radius: 16px;
  background: var(--accent-dim);
}

.status-glyph svg {
  width: 28px;
  height: 28px;
}

.error-glyph {
  color: var(--offline);
  border-color: rgba(242, 118, 107, 0.28);
  background: rgba(242, 118, 107, 0.10);
}

.spin {
  animation: spin 1s linear infinite;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}

.status-title {
  margin: 0 0 8px;
  font-size: 18px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.status-copy {
  margin: 0;
  max-width: 30ch;
  font-size: 13.5px;
  line-height: 1.6;
  color: var(--text-muted);
}

.mono {
  font-family: var(--font-mono);
  color: var(--text);
}

.status-tag {
  margin-top: 18px;
  padding: 3px 11px;
  font-family: var(--font-mono);
  font-size: 11px;
  letter-spacing: 0.02em;
  color: var(--accent-strong);
  border: 1px solid rgba(62, 207, 174, 0.28);
  border-radius: 999px;
  background: var(--accent-dim);
}

.error-tag {
  color: var(--offline);
  border-color: rgba(242, 118, 107, 0.28);
  background: rgba(242, 118, 107, 0.12);
}

.retry-btn {
  margin-top: 22px;
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 9px 18px;
  font-size: 13px;
  font-weight: 600;
  color: var(--surface);
  background: var(--accent-strong);
  border: none;
  border-radius: 10px;
  cursor: pointer;
  transition: filter 0.15s ease;
}

.retry-btn:hover {
  filter: brightness(1.1);
}

.status-flag {
  width: 100%;
  margin-top: 26px;
  padding: 15px 0 20px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  font-size: 12px;
  letter-spacing: 0.03em;
  color: var(--text-faint);
  border-top: 1px solid var(--border);
}

.flag-dot {
  background: var(--offline);
  box-shadow: 0 0 8px rgba(242, 118, 107, 0.35);
}

.flag-live {
  background: var(--accent-strong);
  box-shadow: 0 0 8px rgba(93, 252, 190, 0.45);
}

.flag-offline {
  background: var(--offline);
}

/* ----- Workers list -------------------------------------------------------- */
.workers-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 18px;
}

.workers-title {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.workers-count {
  margin: 0;
  font-size: 20px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.workers-sub {
  margin: 0;
  font-size: 12.5px;
  color: var(--text-muted);
}

.workers-live {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  font-size: 11px;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--accent-strong);
}

.worker-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 12px;
  max-height: none;
}

.worker-item {
  margin: 0;
}

/* ----- Footer / heartbeat -------------------------------------------------- */
.footer {
  display: grid;
  grid-template-columns: 1fr auto 1fr;
  align-items: center;
  gap: 12px;
  padding: 15px 22px;
  border-top: 1px solid var(--border);
  font-size: 12px;
  color: var(--text-muted);
}

.footer-meta {
  font-size: 11.5px;
  letter-spacing: 0.04em;
  color: var(--text-faint);
}

.footer-meta:last-child {
  text-align: right;
}

.footer-heartbeat {
  display: inline-flex;
  align-items: center;
  gap: 10px;
}

.footer-clock {
  font-family: var(--font-mono);
  font-size: 13.5px;
  letter-spacing: 0.08em;
  color: var(--text);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.dot {
  flex: 0 0 auto;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--text-faint);
}

.heartbeat-dot.is-live {
  background: var(--accent-strong);
  box-shadow: 0 0 0 0 rgba(62, 207, 174, 0.45);
  animation: heartbeat-pulse 2.4s ease-out infinite;
}

@keyframes heartbeat-pulse {
  0% {
    box-shadow: 0 0 0 0 rgba(62, 207, 174, 0.45);
  }
  70% {
    box-shadow: 0 0 0 6px rgba(62, 207, 174, 0);
  }
  100% {
    box-shadow: 0 0 0 0 rgba(62, 207, 174, 0);
  }
}
</style>