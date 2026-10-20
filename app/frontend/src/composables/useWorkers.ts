import { computed } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'
import type { WorkerView } from '@bindings/github.com/seppaleinen/infermesh/app/models'
import { useRouterPoll } from './useRouterPoll'

export function useWorkers() {
	const { state, routerURL, start, stop, retry } = useRouterPoll<WorkerView[]>(
		(_) => RouterClient.GetWorkers()
	)

	// The router's /v1/workers projection iterates an in-memory capability
	// cache (a Go map), so the wire order is non-deterministic across polls.
	// If the template bound the raw array directly, every 5s poll would
	// re-order the list, and because Vue reuses <li> nodes by :key="w.id"
	// while the surrounding v-if (expandedWorkerId === w.id) is evaluated
	// against the freshly reassigned array, expanded worker cards would
	// collapse and the rows would jump around (issue #153).
	//
	// Fix: expose a *stable* ordering. Sort once per poll by a deterministic
	// key (worker id, falling back to hostname). The order never depends on
	// map iteration order, so the v-for keys line up identically across
	// refreshes and expanded rows stay mounted. No user-controlled sort
	// column exists today; if one is added later, sort only on explicit
	// user action, never on the automatic poll.
	const stableWorkers = computed(() => {
		if (state.value.kind !== 'ready' || !state.value.data) return []
		return [...state.value.data].sort((a, b) => {
			const ka = a.id || a.hostname || ''
			const kb = b.id || b.hostname || ''
			return ka.localeCompare(kb)
		})
	})

	return { state, routerURL, start, stop, retry, stableWorkers }
}

/**
 * Returns a friendly relative time string for the given timestamp.
 * @param timestamp - ISO timestamp string (e.g. "2026-10-06T10:30:00Z")
 * @returns string like "just now", "5s ago", "2m ago", etc.
 */
export function relativeLastSeen(timestamp: string): string {
	const now = new Date()
	const then = new Date(timestamp)
	const diffMs = now.getTime() - then.getTime()
	const diffSec = Math.floor(diffMs / 1000)

	if (diffSec < 0) return 'just now' // future timestamps shouldn't happen
	if (diffSec < 10) return 'just now'
	if (diffSec < 60) return `${diffSec}s ago`
	const diffMin = Math.floor(diffSec / 60)
	if (diffMin < 60) return `${diffMin}m ago`
	const diffHour = Math.floor(diffMin / 60)
	if (diffHour < 24) return `${diffHour}h ago`
	const diffDay = Math.floor(diffHour / 24)
	return `${diffDay}d ago`
}

/**
 * Formats the given timestamp as an absolute local time string.
 * @param timestamp - ISO timestamp string
 * @returns string like "Oct 6, 10:30 AM"
 */
export function formatAbsolute(timestamp: string): string {
	const date = new Date(timestamp)
	return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) +
		' ' + date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}