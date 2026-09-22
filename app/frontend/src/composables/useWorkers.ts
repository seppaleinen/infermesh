import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '../../bindings/github.com/seppaleinen/infermesh/app'
import type { WorkerView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'

// Poll cadence mirrors the router's heartbeat window (5s) so a worker that
// just went away is visible for at most one tick before the card drops.
const POLL_INTERVAL_MS = 5000
const FIRST_LOAD_TIMEOUT_MS = 4000

export type WorkersState =
  | { kind: 'loading' }
  | { kind: 'empty'; routerURL: string }
  | { kind: 'ready'; workers: WorkerView[]; routerURL: string }
  | { kind: 'error'; routerURL: string; message: string }

function isNetworkError(message: string): boolean {
  const m = message.toLowerCase()
  return (
    m.includes('unreachable') ||
    m.includes('connection') ||
    m.includes('refused') ||
    m.includes('timeout') ||
    m.includes('fetch') ||
    m.includes('network') ||
    m.includes('dial')
  )
}

export function useWorkers() {
  const state = ref<WorkersState>({ kind: 'loading' })
  const routerURL = ref('')
  const firstLoadTimedOut = ref(false)

  let timer: ReturnType<typeof setInterval> | undefined
  let activeFetch: Promise<void> | undefined

  async function fetchOnce(opts: { timeoutMs?: number } = {}): Promise<void> {
    const timeoutMs = opts.timeoutMs ?? 5000
    const controller = new AbortController()
    const id = setTimeout(() => controller.abort(), timeoutMs)
    try {
      const [url, workers] = await Promise.all([
        RouterClient.GetRouterURL(),
        RouterClient.GetWorkers(),
      ])
      routerURL.value = url
      if (workers === null || workers === undefined) {
        state.value = { kind: 'empty', routerURL: url }
      } else {
        state.value = { kind: 'ready', workers, routerURL: url }
      }
      firstLoadTimedOut.value = false
    } catch (e: unknown) {
      const message = e instanceof Error ? e.message : String(e)
      const url = routerURL.value || 'http://127.0.0.1:8080'
      if (isNetworkError(message)) {
        state.value = { kind: 'error', routerURL: url, message }
      } else {
        state.value = {
          kind: 'error',
          routerURL: url,
          message: message || 'failed to fetch workers',
        }
      }
    } finally {
      clearTimeout(id)
    }
  }

  function retry(): void {
    state.value = { kind: 'loading' }
    void fetchOnce({ timeoutMs: FIRST_LOAD_TIMEOUT_MS })
  }

  function start(): void {
    state.value = { kind: 'loading' }
    void fetchOnce({ timeoutMs: FIRST_LOAD_TIMEOUT_MS })
    timer = setInterval(() => {
      // Don't stack a second poll while the first is still in flight.
      if (activeFetch) return
      activeFetch = fetchOnce().finally(() => {
        activeFetch = undefined
      })
    }, POLL_INTERVAL_MS)
  }

  function stop(): void {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }

  onBeforeUnmount(() => stop())

  return { state, routerURL, firstLoadTimedOut, start, stop, retry }
}

export function relativeLastSeen(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return 'never'
  const d = Date.now() - t
  if (d < 0) return 'just now'
  if (d < 10_000) return `${Math.max(1, Math.floor(d / 1000))}s ago`
  if (d < 60_000) return `${Math.floor(d / 1000)}s ago`
  if (d < 3_600_000) return `${Math.floor(d / 60_000)}m ago`
  return `${Math.floor(d / 3_600_000)}h ago`
}

export function formatAbsolute(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  return new Date(t).toLocaleString()
}