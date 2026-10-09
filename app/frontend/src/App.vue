<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Events } from '@wailsio/runtime'
import WorkerCard from './components/WorkerCard.vue'
import SettingsForm from './components/SettingsForm.vue'
import PopularModelsTable from './components/PopularModelsTable.vue'
import { useWorkers, relativeLastSeen, formatAbsolute } from './composables/useWorkers'
import { usePopularModels } from './composables/usePopularModels'
import { useConnectionCount } from './composables/useConnectionCount'
import { useSettings } from './composables/useSettings'
import { useSupervisor } from './composables/useSupervisor'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'

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

// Popular-models view (issue #79). Mirrors useWorkers — same poll cadence
// and first-load timeout, so the table stays in step with the worker list.
const popular = usePopularModels()

// Live client count (issue #79 v3 R1). Mirrors useWorkers — same poll cadence
// and first-load timeout, so the connection count stays in step with the
// worker list. Surface next to the "router online · X workers" badge.
const connectionCount = useConnectionCount()

// Settings view (issue #46). Loaded on mount so we can detect the local
// worker (the one this desktop is running) and tag its card with "You".
const settings = useSettings()

// Own hostname for local-worker identification (issue #90). Matches workers
// by hostname first, falls back to port when hostname is unavailable.
const localHostname = ref<string | null>(null)

// Active view: 'dashboard' (workers) or 'settings'.
const activeView = ref<'dashboard' | 'settings'>('dashboard')

// Forward-only guard for future modals: while a modal is open, Escape must
// not navigate away from the settings view. No modals exist today.
const modalOpen = ref(false)

// Toggle for worker details section
const showWorkerDetails = ref(false)
// Track which worker is expanded in the list
const expandedWorkerId = ref<string | null>(null)

function onKeydown(e: KeyboardEvent): void {
  if (e.key !== 'Escape' || modalOpen.value) return
  if (activeView.value === 'settings') {
    e.preventDefault()
    activeView.value = 'dashboard' // equals Cancel: silent discard; v-if unmount clears form state
  }
}

function onSettingsSaved(): void {
  activeView.value = 'dashboard'
  workers.retry() // re-fetch from new router URL immediately
  popular.retry() // popular models depend on the same router URL
}

// A worker is "local" when its hostname matches this desktop's hostname;
// falls back to port match when hostname is unavailable (issue #90).
// Tags the card with a "You" badge so the user can instantly recognise the
// worker they are running.
const isLocalWorker = (w: { hostname?: string | null; port: number | null }): boolean => {
  if (!settings.settings.value) return false
  const ownPort = settings.settings.value.worker_port
  if (localHostname.value && w.hostname) {
    // Primary match: hostname (case-insensitive)
    return w.hostname.toLowerCase() === localHostname.value.toLowerCase()
  }
  // Fallback: port match when hostname detection is unavailable
  return w.port === ownPort
}

const supervisor = useSupervisor()

const workerState = computed(() => {
  const status = supervisor.status.value
  if (!status) return null
  return status.worker?.state ?? null
})
const workerRegistered = computed(() => {
  const status = supervisor.status.value
  if (!status) return false
  return status.worker?.registered ?? false
})
const routerState = computed(() => {
  const status = supervisor.status.value
  if (!status) return null
  return status.router?.state ?? null
})

async function handleRestartWorker() {
  try {
    await supervisor.restartWorker()
  } catch (e) {
    console.error('restart worker failed', e)
  }
}

async function handleRestartRouter() {
  try {
    await supervisor.restartRouter()
  } catch (e) {
    console.error('restart router failed', e)
  }
}

onMounted(async () => {
  window.addEventListener('keydown', onKeydown)
  unsubscribe = Events.On('time', (ev: { data: string }) => {
    heartbeatText.value = ev.data
    heartbeatAlive.value = true
    scheduleStale()
  })
  workers.start()
  popular.start()
  connectionCount.start()
  await settings.load()
  // Detect own hostname for local-worker identification (issue #90)
  try {
    localHostname.value = await RouterClient.GetHostname()
  } catch {
    localHostname.value = null
  }
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  unsubscribe?.()
  if (staleTimer !== undefined) clearTimeout(staleTimer)
  workers.stop()
  popular.stop()
  connectionCount.stop()
})

const workerCount = computed(() => {
	if (workers.state.value.kind === 'ready') return workers.state.value.data.length
	return 0
})

// Workers data for template
const workersData = computed(() => {
	if (workers.state.value.kind === 'ready') return workers.state.value.data
	return []
})

// Total model instances across all workers (sum of loaded_models.length)
const totalModelInstances = computed(() => {
	if (workers.state.value.kind === 'ready') {
		return workers.state.value.data.reduce((sum, w) => sum + (w.loaded_models?.length || 0), 0)
	}
	return 0
})

const availableModelCount = computed(() => {
	if (popular.state.value.kind === 'ready' && popular.state.value.data.length) {
		return popular.state.value.data.filter(m => m.loaded_worker_count > 0).length
	}
	return 0
})

// Pagination for models
const modelPage = ref(1)
const modelPageSize = ref(10)

const totalModels = computed(() => {
	if (popular.state.value.kind === 'ready') {
		return popular.state.value.data.length
	}
	return 0
})

const totalModelPages = computed(() => {
	return Math.max(1, Math.ceil(totalModels.value / modelPageSize.value))
})

const paginatedModels = computed(() => {
	if (popular.state.value.kind !== 'ready') return []
	const start = (modelPage.value - 1) * modelPageSize.value
	const end = start + modelPageSize.value
	return popular.state.value.data.slice(start, end)
})

function nextModelPage() {
	if (modelPage.value < totalModelPages.value) modelPage.value++
}

function prevModelPage() {
	if (modelPage.value > 1) modelPage.value--
}
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

    <main class="main" :class="{ 'main--settings': activeView === 'settings' }">
      <SettingsForm
        v-if="activeView === 'settings'"
        @saved="onSettingsSaved"
        @cancel="activeView = 'dashboard'"
      />
      <div v-else class="content">
        <!-- Left sidebar: pool stats + supervisor -->
        <aside class="sidebar">
          <div class="sidebar-block stats-block">
            <div class="stat">
              <span class="stat-value">{{ workerCount }}</span>
              <span class="stat-label">worker<span v-if="workerCount !== 1">s</span></span>
            </div>
            <div class="stat">
              <span class="stat-value">{{ totalModels }}</span>
              <span class="stat-label">models</span>
            </div>
            <div class="stat">
              <span class="stat-value">{{ availableModelCount }}</span>
              <span class="stat-label">available</span>
            </div>
            <div class="stat">
              <span class="stat-value">{{ totalModelInstances }}</span>
              <span class="stat-label">instances</span>
            </div>
          </div>

          <div class="sidebar-block router-block" v-if="workers.state.value.kind !== 'loading'">
            <div class="router-label">router</div>
            <div class="router-url mono">{{ workers.state.value.routerURL || '—' }}</div>
            <div v-if="connectionCount.state.value.kind === 'ready'" class="router-connections">
              {{ connectionCount.state.value.data }} connection<span v-if="connectionCount.state.value.data !== 1">s</span>
            </div>
          </div>

          <!-- Supervisor controls (compact) -->
          <div class="sidebar-block supervisor-block" v-if="workerState !== null">
            <div class="supervisor-title">Supervisor</div>
            <div class="supervisor-status">
              <span class="supervisor-label">Worker</span>
              <span class="supervisor-value" :class="{ 'running': workerState === 'running' }">
                {{ workerState }}
              </span>
            </div>
            <div class="supervisor-actions">
              <button class="supervisor-btn" @click="handleRestartWorker" :disabled="workerState !== 'running'" title="Restart Worker">
                <svg viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
                  <path d="M3 12a9 9 0 0 1 9-9 9 9 0 0 1 6.4 2.6M3 5v4h4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                  <path d="M21 12a9 9 0 0 1-9 9 9 9 0 0 1-6.4-2.6M21 19v-4h-4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
                Worker
              </button>
              <button class="supervisor-btn" @click="handleRestartRouter" :disabled="routerState !== 'running'" title="Restart Router">
                <svg viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
                  <path d="M3 12a9 9 0 0 1 9-9 9 9 0 0 1 6.4 2.6M3 5v4h4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                  <path d="M21 12a9 9 0 0 1-9 9 9 9 0 0 1-6.4-2.6M21 19v-4h-4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
                Router
              </button>
            </div>
          </div>
        </aside>

        <!-- Main content: states + models list -->
        <section class="main-panel">
          <!-- Loading: first GetWorkers() in flight -->
          <div v-if="workers.state.value.kind === 'loading'" class="status-card" aria-live="polite">
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
          </div>

          <!-- Router reachable, 0 workers -->
          <div v-else-if="workers.state.value.kind === 'empty'" class="status-card" aria-live="polite">
            <div class="status-glyph" aria-hidden="true">
              <svg viewBox="0 0 24 24" fill="none">
                <circle cx="5" cy="12" r="2.2" stroke="currentColor" stroke-width="1.5" />
                <circle cx="19" cy="6" r="2.2" stroke="currentColor" stroke-width="1.5" />
                <circle cx="19" cy="18" r="2.2" stroke="currentColor" stroke-width="1.5" />
                <path d="M7 11l9.5-3.5M7 13l9.5 3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" opacity="0.6" />
              </svg>
            </div>
            <h1 id="status-title" class="status-title">{{ workerState === 'running' && !workerRegistered ? 'Worker enrolling…' : 'No workers connected' }}</h1>
            <p class="status-copy">
              Router is reachable at <span class="mono">{{ workers.state.value.routerURL }}</span>,
              <template v-if="workerState === 'running' && !workerRegistered">
                but the worker is enrolling and hasn't registered yet.
              </template>
              <template v-else>
                but no worker has registered yet. Start a worker to join the pool.
              </template>
            </p>
            <span class="status-tag">pool dashboard — issue #41</span>
            <div class="status-flag" role="status">
              <span class="dot flag-dot flag-live"></span>
              <span>router online · 0 workers</span>
            </div>
          </div>

          <!-- Router unreachable -->
          <div v-else-if="workers.state.value.kind === 'error'" class="status-card" aria-live="assertive">
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
          </div>

          <!-- Workers present: models list (scrollable + paginated) -->
          <div v-else class="models-panel">
            <div class="models-head">
              <h2 class="models-title">Models</h2>
              <span class="models-count" v-if="totalModels">{{ totalModels }} total</span>
            </div>
            <PopularModelsTable
              v-if="totalModels > 0"
              class="models-table-wrap"
              :models="paginatedModels"
            />
            <div v-else class="models-empty">No models advertised yet</div>

            <!-- Pagination -->
            <div class="pagination" v-if="totalModelPages > 1">
              <button class="page-btn" @click="prevModelPage" :disabled="modelPage <= 1">
                <svg viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
                  <path d="M15 18l-6-6 6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
              </button>
              <span class="page-indicator mono">{{ modelPage }} / {{ totalModelPages }}</span>
              <button class="page-btn" @click="nextModelPage" :disabled="modelPage >= totalModelPages">
                <svg viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
                  <path d="M9 18l6-6-6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                </svg>
              </button>
            </div>
          </div>
        </section>

        <!-- Right: worker details (togglable) -->
        <aside class="workers-panel">
          <button
            class="workers-toggle"
            @click="showWorkerDetails = !showWorkerDetails"
            :aria-expanded="showWorkerDetails"
          >
            <span class="workers-toggle-label">Workers</span>
            <span class="workers-toggle-count">{{ workerCount }}</span>
            <svg class="workers-toggle-chev" :class="{ 'is-open': showWorkerDetails }" viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
              <path d="M9 18l6-6-6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
            </svg>
          </button>

          <div class="workers-panel-body" v-if="showWorkerDetails">
            <ul class="worker-hostname-list">
              <li v-for="w in workersData" :key="w.id" class="worker-hostname-item">
                <button
                  class="worker-hostname-btn"
                  @click="expandedWorkerId = expandedWorkerId === w.id ? null : w.id"
                >
                  <div class="worker-hostname-row">
                    <span class="worker-hostname">{{ w.hostname || 'unknown' }}</span>
                    <span class="worker-status-dot" :class="{ available: w.status === 'available', busy: w.status === 'busy', idle: w.status !== 'available' && w.status !== 'busy' }"></span>
                    <svg class="worker-expand-chev" :class="{ 'is-open': expandedWorkerId === w.id }" viewBox="0 0 24 24" fill="none" width="14" height="14" aria-hidden="true">
                      <path d="M9 18l6-6-6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
                    </svg>
                  </div>
                </button>
                <div v-if="expandedWorkerId === w.id" class="worker-details">
                  <WorkerCard
                    :id="w.id"
                    :address="w.address"
                    :hostname="w.hostname"
                    :status="w.status"
                    :version="w.version"
                    :loaded-models="w.loaded_models"
                    :last-seen-rel="relativeLastSeen(w.last_seen)"
                    :last-seen-absolute="formatAbsolute(w.last_seen)"
                    :is-local="isLocalWorker(w)"
                  />
                </div>
              </li>
            </ul>
            <div v-if="workers.state.value.kind !== 'ready'" class="workers-empty">
              No workers connected
            </div>
          </div>
</aside>
      </div>
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
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  z-index: 1000;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 22px;
  border-bottom: 1px solid var(--border);
  background: var(--surface);
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

/* ----- Main / dashboard layout -------------------------------------------- */
.main {
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 20px 20px 20px 20px;
  padding-top: 72px; /* account for fixed header */
}

.main--settings {
  align-items: flex-start;
  justify-content: flex-start;
  overflow-y: auto;
  --wails-draggable: no-drag; /* interactive form must not start window drags */
  overscroll-behavior: contain;
}

.content {
  width: 100%;
  height: 100%;
  display: grid;
  grid-template-columns: 220px minmax(0, 1fr) 450px;
  gap: 14px;
  min-height: 0;
}

/* ----- Left sidebar -------------------------------------------------------- */
.sidebar {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 0;
  overflow-y: auto;
  --wails-draggable: no-drag;
}

.sidebar-block {
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface);
  padding: 12px 14px;
}

.stats-block {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px 10px;
}

.stat {
  display: flex;
  flex-direction: column;
  gap: 1px;
}

.stat-value {
  font-family: var(--font-mono);
  font-size: 20px;
  font-weight: 650;
  letter-spacing: -0.01em;
  font-variant-numeric: tabular-nums;
  color: var(--text);
}

.stat-label {
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-faint);
}

.router-label {
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-faint);
  margin-bottom: 4px;
}

.router-url {
  font-size: 11.5px;
  word-break: break-all;
  color: var(--text);
}

.router-connections {
  margin-top: 6px;
  font-size: 11.5px;
  color: var(--text-muted);
}

/* Compact supervisor block */
.supervisor-block {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.supervisor-block .supervisor-title {
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-faint);
}

.supervisor-status {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 12.5px;
}

.supervisor-label {
  color: var(--text-faint);
}

.supervisor-value {
  font-weight: 600;
  text-transform: capitalize;
}

.supervisor-value.running {
  color: var(--accent-strong);
}

.supervisor-actions {
  display: flex;
  gap: 8px;
}

.supervisor-btn {
  flex: 1;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 6px 8px;
  font-size: 12px;
  font-weight: 600;
  color: var(--text);
  background: var(--surface-2);
  border: 1px solid var(--border-strong);
  border-radius: 8px;
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease;
}

.supervisor-btn:hover:not(:disabled) {
  color: var(--surface);
  background: var(--accent-strong);
  border-color: var(--accent-strong);
}

.supervisor-btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

/* ----- Main panel (models) -------------------------------------------------- */
.main-panel {
  min-width: 0;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface);
  padding: 18px 20px;
}

.models-panel {
  width: 100%;
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.models-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  margin-bottom: 10px;
}

.models-title {
  margin: 0;
  font-size: 14px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.models-count {
  font-size: 11.5px;
  color: var(--text-faint);
  font-family: var(--font-mono);
}

.models-table-wrap {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  --wails-draggable: no-drag;
  overscroll-behavior: contain;
}

.models-empty {
  flex: 1;
  display: grid;
  place-items: center;
  color: var(--text-faint);
  font-style: italic;
  font-size: 13px;
}

/* Pagination */
.pagination {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 12px;
  margin-top: 12px;
  padding-top: 12px;
  border-top: 1px solid var(--border);
}

.page-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  color: var(--text-muted);
  background: var(--surface-2);
  border: 1px solid var(--border-strong);
  border-radius: 8px;
  cursor: pointer;
  transition: color 0.15s ease, background 0.15s ease;
}

.page-btn:hover:not(:disabled) {
  color: var(--accent-strong);
}

.page-btn:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}

.page-indicator {
  font-size: 12px;
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
}

/* ----- Workers panel (right, hostnames with expand) ------------------------------------ */
.workers-panel {
  position: relative;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface);
  overflow: hidden;
}

.workers-toggle {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 14px 16px;
  font-size: 13px;
  font-weight: 650;
  color: var(--text);
  background: transparent;
  border: none;
  border-bottom: 1px solid var(--border);
  cursor: pointer;
  transition: background 0.15s ease;
}

.workers-toggle:hover {
  background: var(--surface-2);
}

.workers-toggle-label {
  flex: 1;
  text-align: left;
}

.workers-toggle-count {
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: var(--text-muted);
  padding: 2px 8px;
  border: 1px solid var(--border-strong);
  border-radius: 990px;
  background: var(--surface-2);
}

.workers-toggle-chev {
  color: var(--text-faint);
  transition: transform 0.2s ease;
}

.workers-toggle-chev.is-open {
  transform: rotate(90deg);
}

.workers-panel-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 0;
  --wails-draggable: no-drag;
  overscroll-behavior: contain;
}

.worker-hostname-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}

.worker-hostname-item {
  border-bottom: 1px solid var(--border);
}

.worker-hostname-item:last-child {
  border-bottom: none;
}

.worker-hostname-btn {
  width: 100%;
  padding: 12px 16px;
  background: transparent;
  border: none;
  text-align: left;
  cursor: pointer;
  transition: background 0.15s ease;
}

.worker-hostname-btn:hover {
  background: var(--surface-2);
}

.worker-hostname-row {
  display: flex;
  align-items: center;
  gap: 10px;
}

.worker-hostname {
  flex: 1;
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.worker-status-dot {
  flex: 0 0 auto;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--text-faint);
}

.worker-status-dot.available {
  background: var(--accent-strong);
}

.worker-status-dot.busy {
  background: #f0b429;
}

.worker-status-dot.idle {
  background: var(--text-faint);
}

.worker-expand-chev {
  color: var(--text-faint);
  transition: transform 0.2s ease;
}

.worker-expand-chev.is-open {
  transform: rotate(90deg);
}

.worker-details {
  padding: 0 16px 16px 16px;
  border-top: 1px solid var(--border);
}

.workers-empty {
  text-align: center;
  color: var(--text-faint);
  font-style: italic;
  font-size: 13px;
  padding: 20px;
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

/* Worker list (used in right panel) */
.worker-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.worker-item {
  margin: 0;
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