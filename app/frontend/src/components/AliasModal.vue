<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import type { PopularModelView } from '@bindings/github.com/seppaleinen/infermesh/app/models'

const props = defineProps<{
  popularModels: PopularModelView[]
}>()

const emit = defineEmits<{
  confirm: [payload: { alias: string; canonical: string }]
  cancel: []
}>()

const alias = ref('')
const canonical = ref('')
const aliasInput = ref<HTMLInputElement | null>(null)

const canSave = computed(() => alias.value.trim() !== '' && canonical.value !== '')

function onConfirm(): void {
  if (!canSave.value) return
  emit('confirm', { alias: alias.value.trim(), canonical: canonical.value })
}

function onCancel(): void {
  emit('cancel')
}

function reset(): void {
  alias.value = ''
  canonical.value = ''
}

// Autofocus alias input on mount
onMounted(() => {
  aliasInput.value?.focus()
})

// Reset when modal reopens (v-if toggling)
watch(() => props.popularModels, () => {
  // no-op: just triggers reactivity
})
</script>

<template>
  <div class="alias-modal-backdrop" role="presentation">
    <div
      class="alias-modal"
      role="dialog"
      aria-modal="true"
      aria-labelledby="alias-modal-title"
    >
      <div class="alias-modal-header">
        <h2 id="alias-modal-title" class="alias-modal-title">Create Model Alias</h2>
        <button class="alias-modal-close" @click="onCancel" aria-label="Close">×</button>
      </div>

      <form @submit.prevent="onConfirm" class="alias-modal-form">
        <div class="alias-field">
          <label for="alias-input" class="alias-label">Alias name</label>
          <input
            id="alias-input"
            ref="aliasInput"
            v-model="alias"
            type="text"
            class="alias-input"
            placeholder="e.g. gemma-4-12b"
            autocomplete="off"
          />
        </div>

        <div class="alias-field">
          <label for="canonical-select" class="alias-label">Canonical model</label>
          <select
            id="canonical-select"
            v-model="canonical"
            class="alias-select"
          >
            <option value="" disabled>Select a model…</option>
            <option
              v-for="m in popularModels"
              :key="m.model"
              :value="m.model"
            >
              {{ m.model }}
              <span v-if="m.loaded_worker_count > 0">({{ m.loaded_worker_count }} loaded)</span>
              <span v-else>({{ m.worker_count }} available)</span>
            </option>
          </select>
        </div>

        <div v-if="alias && canonical" class="alias-preview">
          Requests for <code>{{ alias }}</code> → <code>{{ canonical }}</code>
        </div>

        <div v-if="popularModels.length === 0" class="alias-warning">
          No models available yet. Start a worker or add a model first.
        </div>

        <div class="alias-modal-actions">
          <button type="button" class="alias-btn-cancel" @click="onCancel">Cancel</button>
          <button
            type="submit"
            class="alias-btn-save"
            :disabled="!canSave"
          >Create alias</button>
        </div>
      </form>
    </div>
  </div>
</template>

<style scoped>
.alias-modal-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 1000;
  padding: 16px;
}

.alias-modal {
  background: var(--surface, #1a1a2e);
  border: 1px solid var(--border-strong, #333);
  border-radius: 12px;
  width: 100%;
  max-width: 420px;
  padding: 0;
  box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
}

.alias-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 20px;
  border-bottom: 1px solid var(--border-strong, #333);
}

.alias-modal-title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--text, #e0e0e0);
}

.alias-modal-close {
  background: none;
  border: none;
  color: var(--text-muted, #888);
  font-size: 20px;
  cursor: pointer;
  padding: 4px 8px;
  line-height: 1;
}

.alias-modal-close:hover {
  color: var(--text, #e0e0e0);
}

.alias-modal-form {
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.alias-field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.alias-label {
  font-size: 13px;
  font-weight: 500;
  color: var(--text-muted, #999);
}

.alias-input,
.alias-select {
  background: var(--surface-2, #2a2a3e);
  border: 1px solid var(--border-strong, #444);
  border-radius: 8px;
  padding: 10px 12px;
  color: var(--text, #e0e0e0);
  font-size: 14px;
  font-family: inherit;
  outline: none;
}

.alias-input:focus,
.alias-select:focus {
  border-color: var(--accent, #6366f1);
  box-shadow: 0 0 0 2px rgba(99, 102, 241, 0.2);
}

.alias-input::placeholder {
  color: var(--text-muted, #666);
}

.alias-preview {
  background: var(--surface-2, #2a2a3e);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 13px;
  color: var(--text-muted, #999);
}

.alias-preview code {
  background: rgba(99, 102, 241, 0.15);
  color: var(--accent, #6366f1);
  padding: 2px 6px;
  border-radius: 4px;
  font-size: 12px;
}

.alias-warning {
  font-size: 13px;
  color: var(--text-muted, #888);
  font-style: italic;
}

.alias-modal-actions {
  display: flex;
  gap: 8px;
  justify-content: flex-end;
  padding-top: 8px;
}

.alias-btn-cancel,
.alias-btn-save {
  padding: 8px 16px;
  border-radius: 8px;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  border: none;
  transition: background 0.15s ease, opacity 0.15s ease;
}

.alias-btn-cancel {
  background: var(--surface-2, #2a2a3e);
  color: var(--text, #e0e0e0);
}

.alias-btn-cancel:hover {
  background: var(--surface-3, #3a3a4e);
}

.alias-btn-save {
  background: var(--accent, #6366f1);
  color: white;
}

.alias-btn-save:hover:not(:disabled) {
  background: #5558e6;
}

.alias-btn-save:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}
</style>
