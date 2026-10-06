import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '../../bindings/github.com/seppaleinen/infermesh/app'
import type { PopularModelView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'

// Poll cadence mirrors the router's heartbeat window (5s) so the popular-models
// list refreshes in step with the workers list. First-load timeout is shorter
// (4s) so the loading card resolves before the UI feels stuck.
const POLL_INTERVAL_MS = 5000
const FIRST_LOAD_TIMEOUT_MS = 4000

export type PopularModelsState =
  | { kind: 'loading' }
  | { kind: 'empty'; routerURL: string }
  | { kind: 'ready'; models: PopularModelView[]; routerURL: string }
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

export function usePopularModels() {
  const state = ref<PopularModelsState>({ kind: 'loading' })
  const routerURL = ref('')

  let timer: ReturnType<typeof setInterval> | undefined
  let activeFetch: Promise<void> | undefined

  async function fetchOnce(opts: { timeoutMs?: number } = {}): Promise<void> {
    const timeoutMs = opts.timeoutMs ?? 5000
    const controller = new AbortController()
    const id = setTimeout(() => controller.abort(), timeoutMs)
    try {
      const [url, models] = await Promise.all([
        RouterClient.GetRouterURL(),
        RouterClient.GetPopularModels(),
      ])
      routerURL.value = url
      if (models === null || models === undefined) {
        state.value = { kind: 'empty', routerURL: url }
      } else {
        state.value = { kind: 'ready', models, routerURL: url }
      }
    } catch (e: unknown) {
      const message = e instanceof Error ? e.message : String(e)
      const url = routerURL.value || 'http://127.0.0.1:8080'
      if (isNetworkError(message)) {
        state.value = { kind: 'error', routerURL: url, message }
      } else {
        state.value = {
          kind: 'error',
          routerURL: url,
          message: message || 'failed to fetch popular models',
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

  return { state, routerURL, start, stop, retry }
}