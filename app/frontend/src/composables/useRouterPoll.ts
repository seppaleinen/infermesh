import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'

// Poll cadence mirrors the router's heartbeat window (5s) so the data
// refreshes in step with other polling composables. First-load timeout is shorter
// (4s) so the loading card resolves before the UI feels stuck.
export const POLL_INTERVAL_MS = 5000
export const FIRST_LOAD_TIMEOUT_MS = 4000

/** Returns true if the error message indicates a network-level failure. */
export function isNetworkError(message: string): boolean {
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

/**
 * Generic polling factory for router HTTP endpoints.
 *
 * @param fetchFn - function that calls the RouterClient method for a given endpoint
 * @returns an object with `state`, `routerURL`, `start`, `stop`, `retry`
 */
export function useRouterPoll<T>(
	fetchFn: (ctx: unknown) => Promise<T | null>
) {
	const state = ref<
		| { kind: 'loading' }
		| { kind: 'empty'; routerURL: string }
		| { kind: 'ready'; data: T; routerURL: string }
		| { kind: 'error'; routerURL: string; message: string }
	>({ kind: 'loading' })
	const routerURL = ref('')

	let timer: ReturnType<typeof setInterval> | undefined
	let activeFetch: Promise<void> | undefined

	async function fetchOnce(opts: { timeoutMs?: number } = {}): Promise<void> {
		const timeoutMs = opts.timeoutMs ?? 5000
		const controller = new AbortController()
		const id = setTimeout(() => controller.abort(), timeoutMs)
		try {
			const [url, data] = await Promise.all([
				RouterClient.GetRouterURL(),
				fetchFn({ signal: controller.signal })
			])
			routerURL.value = url
			if (data === null || data === undefined) {
				state.value = { kind: 'empty', routerURL: url }
			} else {
				state.value = { kind: 'ready', data, routerURL: url }
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
					message: message || 'failed to fetch data'
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