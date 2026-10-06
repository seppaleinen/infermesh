<script setup lang="ts">
import type { PopularModelView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'

defineProps<{ models: PopularModelView[] }>()
</script>

<template>
  <table class="popular-table" aria-label="Popular models across the pool">
    <thead>
      <tr>
        <th scope="col">Model</th>
        <th scope="col">Workers</th>
        <th scope="col">Calls (10m)</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="m in models" :key="m.model">
        <td class="model-name">{{ m.model }}</td>
        <td class="model-workers">{{ m.worker_count }}</td>
        <td class="model-calls">{{ m.call_count }}</td>
      </tr>
      <tr v-if="models.length === 0">
        <td colspan="3" class="popular-empty">No models advertised yet</td>
      </tr>
    </tbody>
  </table>
</template>

<style scoped>
.popular-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.popular-table th {
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

.popular-table td {
  padding: 10px 12px;
  border-bottom: 1px solid var(--border);
}

.popular-table tbody tr:last-child td {
  border-bottom: none;
}

.popular-table tbody tr:hover td {
  background: var(--surface-2);
}

.model-name {
  font-family: var(--font-mono);
  font-size: 12.5px;
  color: var(--text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 300px;
}

.model-workers,
.model-calls {
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  color: var(--text-muted);
  text-align: right;
  white-space: nowrap;
}

.popular-empty {
  text-align: center;
  color: var(--text-faint);
  font-style: italic;
  padding: 20px !important;
}
</style>