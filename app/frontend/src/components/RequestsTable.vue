<script setup lang="ts">
import type { InflightRequestView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'

defineProps<{ requests: InflightRequestView[] }>()

function statusClass(status: string): string {
	switch (status) {
		case 'in-flight':
			return 'status-inflight'
		case 'streaming':
			return 'status-streaming'
		case 'done':
			return 'status-done'
		case 'error':
			return 'status-error'
		default:
			return ''
	}
}

function statusLabel(status: string): string {
	switch (status) {
		case 'in-flight':
			return 'In flight'
		case 'streaming':
			return 'Streaming'
		case 'done':
			return 'Done'
		case 'error':
			return 'Error'
		default:
			return status
	}
}

function formatDuration(ms: number | undefined): string {
	if (ms === undefined || ms < 0) return '—'
	if (ms < 1000) return `${ms}ms`
	return `${(ms / 1000).toFixed(2)}s`
}

function formatTime(ts: string): string {
	if (!ts) return '—'
	const d = new Date(ts)
	if (Number.isNaN(d.getTime())) return '—'
	const now = Date.now()
	const diff = now - d.getTime()
	if (diff < 0) return 'just now'
	if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`
	if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
	if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`
	return d.toLocaleString()
}
</script>

<template>
<table class="requests-table" aria-label="In-flight and recent requests">
  <thead>
    <tr>
      <th scope="col" class="col-id">Request</th>
      <th scope="col">Model</th>
      <th scope="col">Worker</th>
      <th scope="col">Kind</th>
      <th scope="col">Started</th>
      <th scope="col">Duration</th>
      <th scope="col">Status</th>
    </tr>
  </thead>
  <tbody>
    <tr v-for="r in requests" :key="r.request_id">
      <td class="col-id">{{ r.request_id }}</td>
      <td class="model-name">{{ r.model }}</td>
      <td class="worker-id">{{ r.worker_id }}</td>
      <td class="kind">{{ r.kind }}</td>
      <td class="time">{{ formatTime(r.started_at) }}</td>
      <td class="duration">{{ formatDuration(r.duration_ms) }}</td>
      <td class="status"><span class="badge" :class="statusClass(r.status)">{{ statusLabel(r.status) }}</span></td>
    </tr>
    <tr v-if="requests.length === 0">
      <td colspan="7" class="requests-empty">No requests recorded yet</td>
    </tr>
  </tbody>
</table>
</template>

<style scoped>
.requests-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.requests-table th {
  text-align: left;
  padding: 10px 12px;
  font-weight: 600;
  font-size: 11px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-faint);
  border-bottom: 1px solid var(--border);
  background: var(--surface);
}

.requests-table td {
  padding: 10px 12px;
  border-bottom: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: 12.5px;
  color: var(--text-muted);
  white-space: nowrap;
}

.requests-table tbody tr:last-child td {
  border-bottom: none;
}

.requests-table tbody tr:hover td {
  background: var(--surface-2);
}

.col-id,
.worker-id {
  color: var(--text);
}

.model-name {
  color: var(--text);
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
}

.kind {
  text-transform: lowercase;
}

.time,
.duration {
  color: var(--text-faint);
}

.status .badge {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 10px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  white-space: nowrap;
}

.status-inflight {
  background: rgba(59, 130, 246, 0.15);
  color: #60a5fa;
}

.status-streaming {
  background: rgba(168, 85, 247, 0.15);
  color: #c084fc;
}

.status-done {
  background: rgba(34, 197, 94, 0.15);
  color: #4ade80;
}

.status-error {
  background: rgba(239, 68, 68, 0.15);
  color: #f87171;
}

.requests-empty {
  text-align: center;
  color: var(--text-faint);
  font-style: italic;
  padding: 20px !important;
}
</style>