<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Events } from '@wailsio/runtime'

// Static placeholder — replaced by a real version query when the pool
// dashboard lands in #41. No Go binding involved.
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

onMounted(() => {
  unsubscribe = Events.On('time', (ev: { data: string }) => {
    heartbeatText.value = ev.data
    heartbeatAlive.value = true
    scheduleStale()
  })
})

onBeforeUnmount(() => {
  unsubscribe?.()
  if (staleTimer !== undefined) clearTimeout(staleTimer)
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
      <span class="badge">{{ version }}</span>
    </header>

    <main class="main">
      <section class="status-card" aria-labelledby="status-title">
        <div class="status-glyph" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none">
            <circle cx="5" cy="12" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <circle cx="19" cy="6" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <circle cx="19" cy="18" r="2.2" stroke="currentColor" stroke-width="1.5" />
            <path d="M7 11l9.5-3.5M7 13l9.5 3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" opacity="0.6" />
          </svg>
        </div>
        <h1 id="status-title" class="status-title">No router connected</h1>
        <p class="status-copy">Workers stay idle until a router announces on the local network.</p>
        <span class="status-tag">pool dashboard — issue #41</span>
        <div class="status-flag" role="status">
          <span class="dot flag-dot"></span>
          <span>router offline</span>
        </div>
      </section>
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
  border-radius: 999px;
  background: var(--surface-2);
}

/* ----- Main / status placeholder ------------------------------------------- */
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