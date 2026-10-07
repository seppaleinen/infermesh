import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'
import type { WorkerView } from '@bindings/github.com/seppaleinen/infermesh/app/models'
import { useRouterPoll } from './useRouterPoll'

export function useWorkers() {
	const { state, routerURL, start, stop, retry } = useRouterPoll<WorkerView[]>(
		(_) => RouterClient.GetWorkers()
	)
	return { state, routerURL, start, stop, retry }
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