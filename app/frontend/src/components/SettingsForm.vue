<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  ConfigService,
  RouterClient,
  type Settings,
} from '../../bindings/github.com/seppaleinen/infermesh/app'
import { useSettings } from '../composables/useSettings'

const {
  settings,
  loading,
  error,
  isKeyringAvailable,
  load,
  save,
  refreshKeyring,
  getSecret,
  deleteSecret,
} = useSettings()

const emit = defineEmits({
  saved: null,
  cancel: null,
})

// Local secret refs — NEVER persisted to localStorage/sessionStorage
const routerApikey = ref('')
const workerCustomAuth = ref('')
const workerMTLSCert = ref('')
const workerMTLSKey = ref('')

const saving = ref(false)
const saveError = ref<string | null>(null)
const keyringServiceName = ref('infermesh')

// Show/hide password toggles
const showRouterApikey = ref(false)
const showWorkerCustomAuth = ref(false)
const showWorkerMTLSCert = ref(false)
const showWorkerMTLSKey = ref(false)

// Snapshot for dirty tracking — taken at last load() / successful save()
const snapshot = ref<Settings | null>(null)
const snapshotSecrets = ref<Record<string, string>>({})

async function loadSettings(): Promise<void> {
  saveError.value = null
  try {
    const s = await load()
    if (s) {
      takeSnapshot(s)
      // Fetch service name for the keyring banner
      try {
        keyringServiceName.value = await ConfigService.GetKeyringServiceName()
      } catch {
        // fallback stays 'infermesh'
      }
      // Read back any secrets already stored in the keyring so the inputs
      // show them (a blank field means "no secret set", not "secret hidden").
      await loadExistingSecrets()
    }
  } catch {
    // error ref is already populated by useSettings
  }
}

async function loadExistingSecrets(): Promise<void> {
  const refs = [
    { ref: 'router/apikey', target: routerApikey },
    { ref: 'worker/customauth', target: workerCustomAuth },
    { ref: 'mtls/cert', target: workerMTLSCert },
    { ref: 'mtls/key', target: workerMTLSKey },
  ]
  for (const { ref, target } of refs) {
    try {
      target.value = await getSecret(ref)
    } catch {
      // getSecret already swallowed errNotFound → ''; a real error here
      // is surfaced via the composable's error ref, not the field.
    }
  }
}

function takeSnapshot(s: Settings): void {
  // Deep clone settings to avoid reference aliasing. Secrets are stripped
  // before snapshotting so a secret value never lingers in the reactive
  // snapshot object after a successful save.
  const clean: Settings = { ...s, secrets: {} }
  snapshot.value = JSON.parse(JSON.stringify(clean))
  snapshotSecrets.value = {
    routerApikey: '',
    workerCustomAuth: '',
    workerMTLSCert: '',
    workerMTLSKey: '',
  }
}

// Dirty tracking: compare current settings + secret refs against snapshot
const dirty = computed(() => {
  if (!settings.value || !snapshot.value) return false
  if (saving.value || loading.value) return false

  const keys: (keyof Settings)[] = [
    'router_addr',
    'worker_backend',
    'worker_model_path',
    'worker_port',
    'worker_addr',
    'worker_apikey',
    'worker_custom_auth',
    'worker_mtls_cert',
    'worker_mtls_key',
    'relay_url',
    'dev_mode',
    'worker_enable_health_checks',
  ]

  for (const k of keys) {
    if (settings.value[k] !== snapshot.value[k]) return true
  }

  // Secret refs (local)
  if (routerApikey.value !== snapshotSecrets.value.routerApikey) return true
  if (workerCustomAuth.value !== snapshotSecrets.value.workerCustomAuth) return true
  if (workerMTLSCert.value !== snapshotSecrets.value.workerMTLSCert) return true
  if (workerMTLSKey.value !== snapshotSecrets.value.workerMTLSKey) return true

  return false
})

async function handleSave(): Promise<void> {
  if (!settings.value) return

  saving.value = true
  saveError.value = null
  await refreshKeyring()

  // Assemble secrets object from local refs
  const secrets: { [key: string]: string } = {}
  if (routerApikey.value) secrets['router/apikey'] = routerApikey.value
  if (workerCustomAuth.value) secrets['worker/customauth'] = workerCustomAuth.value
  if (workerMTLSCert.value) secrets['mtls/cert'] = workerMTLSCert.value
  if (workerMTLSKey.value) secrets['mtls/key'] = workerMTLSKey.value

  const s: Settings = { ...settings.value, secrets }

  const hadSecrets = Object.keys(s.secrets || {}).length > 0

  // A blank local secret field means "remove it": if the ref was already
  // persisted (present in SecretRefs on disk), call DeleteSecret so the
  // keyring entry and the YAML reference are dropped. This is the only way
  // to remove a secret through the UI — there is no separate Clear button.
  const localSecrets: Record<string, string> = {
    'router/apikey': routerApikey.value,
    'worker/customauth': workerCustomAuth.value,
    'mtls/cert': workerMTLSCert.value,
    'mtls/key': workerMTLSKey.value,
  }
  const persistedRefs = settings.value?.secret_refs || {}
  for (const [ref, value] of Object.entries(localSecrets)) {
    if (value === '' && persistedRefs[ref]) {
      try {
        await deleteSecret(ref)
      } catch {
        // Non-fatal: the composable surfaces it via error ref; we keep
        // going so the non-secret fields still save.
      }
    }
  }

  try {
    const keyringAvailable = await save(s)

    // Differentiate: the Go side persists non-secret fields even when the
    // keyring is down (returns false, nil). Only block when secrets were
    // actually present — otherwise the user's router/backend changes were
    // written and we must not pretend they weren't.
    if (!keyringAvailable) {
      if (hadSecrets) {
        saveError.value =
          'Keyring is unavailable — secrets were NOT saved. Non-secret settings were saved; enable a keyring to store secrets.'
        return
      }
      // No secrets: non-secret settings were persisted. Show a non-blocking
      // note rather than an error, then proceed to re-point the dashboard.
      saveError.value = null
    }

    // Re-point the in-memory client so the dashboard talks to the new router
    // immediately (no restart required). SetRouterURL normalises the URL the
    // same way the constructor does.
    if (settings.value) {
      try {
        await RouterClient.SetRouterURL(settings.value.router_addr)
      } catch {
        // Non-fatal: the value is persisted in YAML and takes effect on
        // restart; the workers.retry() in onSettingsSaved will just hit the
        // old URL until then.
      }
    }

    // Clear local secret refs after success
    routerApikey.value = ''
    workerCustomAuth.value = ''
    workerMTLSCert.value = ''
    workerMTLSKey.value = ''

    // Snapshot the persisted non-secret state so dirty tracking compares
    // against what's on disk, not the transient secret values.
    const snap = { ...s, secrets: {} }
    takeSnapshot(snap)
    emit('saved')
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    saveError.value = msg
  } finally {
    saving.value = false
  }
}

function handleCancel(): void {
  // Reset local secret refs
  routerApikey.value = ''
  workerCustomAuth.value = ''
  workerMTLSCert.value = ''
  workerMTLSKey.value = ''
  emit('cancel')
}

onMounted(() => {
  void loadSettings()
})
</script>

<template>
  <div class="settings" :aria-busy="loading || saving || undefined">
    <!-- Keyring unavailable banner -->
    <div v-if="!isKeyringAvailable" class="keyring-banner" role="alert">
      <svg class="keyring-glyph" viewBox="0 0 24 24" fill="none" aria-hidden="true">
        <path d="M12 17.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5Z" stroke="currentColor" stroke-width="1.5" />
        <path d="M6 10a6 6 0 1 1 12 0v3a6 6 0 0 1-12 0V10Z" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      </svg>
      <span>
        Keyring is unavailable — secrets cannot be saved. Service:
        <span class="mono">{{ keyringServiceName }}</span>.
      </span>
    </div>

    <!-- Loading skeleton -->
    <div v-if="loading" class="loading-skeleton">
      <div class="skeleton-shimmer"></div>
    </div>

    <!-- Error card -->
    <div v-else-if="error" class="error-card" role="alert">
      <h2>Settings could not be loaded</h2>
      <p>{{ error }}</p>
      <button class="btn-primary" @click="loadSettings()">
        Retry
      </button>
    </div>

    <!-- Form -->
    <form v-else-if="settings" @submit.prevent="handleSave">
      <section class="form-section">
        <h2>Router</h2>

        <div class="field">
          <label for="router_addr">Router address</label>
          <input
            id="router_addr"
            type="text"
            v-model="settings.router_addr"
            :disabled="saving"
          />
        </div>

        <div class="field checkbox-field">
          <label for="dev_mode">Dev mode</label>
          <input
            id="dev_mode"
            type="checkbox"
            v-model="settings.dev_mode"
            :true-value="true"
            :false-value="false"
            :disabled="saving"
          />
          <span class="field-help"
            >Bypasses mDNS discovery; uses static router URL.</span
          >
        </div>

        <div class="field">
          <label for="router_apikey">Router API key</label>
          <div class="input-group">
            <input
              id="router_apikey"
              :type="showRouterApikey ? 'text' : 'password'"
              v-model="routerApikey"
              :disabled="!isKeyringAvailable || saving"
              autocomplete="off"
            />
            <button
              type="button"
              class="toggle-eye"
              :aria-label="showRouterApikey ? 'Hide' : 'Show'"
              @click="showRouterApikey = !showRouterApikey"
              :disabled="!isKeyringAvailable || saving"
              tabindex="-1"
            >
              <template v-if="showRouterApikey">
                <!-- Eye off -->
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                  <path d="M3 3l18 18" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
                </svg>
              </template>
              <template v-else>
                <!-- Eye on -->
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                </svg>
              </template>
            </button>
          </div>
        </div>
      </section>

      <section class="form-section">
        <h2>Worker</h2>

        <div class="field">
          <label for="worker_backend">Backend</label>
          <select
            id="worker_backend"
            v-model="settings.worker_backend"
            :disabled="saving"
          >
            <option value="llama-cpp">llama-cpp</option>
            <option value="ollama">ollama</option>
            <option value="lmstudio">lmstudio</option>
            <option value="vllm">vllm</option>
            <option value="custom">custom</option>
          </select>
        </div>

        <div class="field">
          <label for="worker_model_path">Model path</label>
          <input
            id="worker_model_path"
            type="text"
            v-model="settings.worker_model_path"
            :disabled="saving"
          />
        </div>

        <div class="field">
          <label for="worker_port">Port</label>
          <input
            id="worker_port"
            type="number"
            min="1"
            max="65535"
            v-model.number="settings.worker_port"
            :disabled="saving"
          />
        </div>

        <div class="field">
          <label for="worker_addr">Worker address</label>
          <input
            id="worker_addr"
            type="text"
            v-model="settings.worker_addr"
            :disabled="saving"
          />
        </div>

        <div class="field">
          <label for="worker_apikey">Worker API key</label>
          <input
            id="worker_apikey"
            type="text"
            v-model="settings.worker_apikey"
            :disabled="saving"
          />
        </div>

        <div
          v-if="settings.worker_backend === 'custom'"
          class="field"
        >
          <label for="worker_custom_auth">Custom auth header</label>
          <div class="input-group">
            <input
              id="worker_custom_auth"
              :type="showWorkerCustomAuth ? 'text' : 'password'"
              v-model="workerCustomAuth"
              :disabled="!isKeyringAvailable || saving"
              autocomplete="off"
            />
            <button
              type="button"
              class="toggle-eye"
              :aria-label="showWorkerCustomAuth ? 'Hide' : 'Show'"
              @click="showWorkerCustomAuth = !showWorkerCustomAuth"
              :disabled="!isKeyringAvailable || saving"
              tabindex="-1"
            >
              <template v-if="showWorkerCustomAuth">
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                  <path d="M3 3l18 18" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
                </svg>
              </template>
              <template v-else>
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                </svg>
              </template>
            </button>
          </div>
        </div>

        <div class="field">
          <label for="worker_mtls_cert">mTLS certificate</label>
          <div class="input-group">
            <input
              id="worker_mtls_cert"
              :type="showWorkerMTLSCert ? 'text' : 'password'"
              v-model="workerMTLSCert"
              :disabled="!isKeyringAvailable || saving"
              autocomplete="off"
            />
            <button
              type="button"
              class="toggle-eye"
              :aria-label="showWorkerMTLSCert ? 'Hide' : 'Show'"
              @click="showWorkerMTLSCert = !showWorkerMTLSCert"
              :disabled="!isKeyringAvailable || saving"
              tabindex="-1"
            >
              <template v-if="showWorkerMTLSCert">
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                  <path d="M3 3l18 18" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
                </svg>
              </template>
              <template v-else>
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                </svg>
              </template>
            </button>
          </div>
        </div>

        <div class="field">
          <label for="worker_mtls_key">mTLS key</label>
          <div class="input-group">
            <input
              id="worker_mtls_key"
              :type="showWorkerMTLSKey ? 'text' : 'password'"
              v-model="workerMTLSKey"
              :disabled="!isKeyringAvailable || saving"
              autocomplete="off"
            />
            <button
              type="button"
              class="toggle-eye"
              :aria-label="showWorkerMTLSKey ? 'Hide' : 'Show'"
              @click="showWorkerMTLSKey = !showWorkerMTLSKey"
              :disabled="!isKeyringAvailable || saving"
              tabindex="-1"
            >
              <template v-if="showWorkerMTLSKey">
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                  <path d="M3 3l18 18" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
                </svg>
              </template>
              <template v-else>
                <svg viewBox="0 0 24 24" fill="none" width="16" height="16">
                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" stroke="currentColor" stroke-width="1.5" />
                  <circle cx="12" cy="12" r="2.5" stroke="currentColor" stroke-width="1.5" />
                </svg>
              </template>
            </button>
          </div>
        </div>

        <div class="field">
          <label for="relay_url">Relay URL</label>
          <input
            id="relay_url"
            type="text"
            v-model="settings.relay_url"
            :disabled="saving"
          />
        </div>

        <div class="field checkbox-field">
          <label for="worker_enable_health_checks">Enable health checks</label>
          <input
            id="worker_enable_health_checks"
            type="checkbox"
            v-model="settings.worker_enable_health_checks"
            :true-value="true"
            :false-value="false"
            :disabled="saving"
          />
        </div>
      </section>

      <!-- Inline save error -->
      <div v-if="saveError" class="save-error" role="alert">
        {{ saveError }}
      </div>

      <!-- Actions -->
      <div class="form-actions">
        <button
          type="submit"
          class="btn-primary"
          :disabled="!dirty || loading || saving"
          :aria-busy="saving || undefined"
        >
          <svg v-if="saving" class="spin" viewBox="0 0 24 24" fill="none" width="16" height="16" aria-hidden="true">
            <circle cx="12" cy="12" r="9" stroke="currentColor" stroke-width="2.5" opacity="0.3" />
            <path d="M12 3a9 9 0 0 1 9 9" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" />
          </svg>
          <span>{{ saving ? 'Saving…' : 'Save' }}</span>
        </button>
        <button
          type="button"
          class="btn-secondary"
          @click="handleCancel"
          :disabled="saving"
        >
          Cancel
        </button>
      </div>
    </form>
  </div>
</template>

<style scoped>
.settings {
  width: 100%;
  max-width: 720px;
  margin: 0 auto;
  padding: 24px;
}

.settings form {
  display: flex;
  flex-direction: column;
  gap: 28px;
}

/* ----- Form sections -------------------------------------------------------- */
.form-section {
  padding: 22px;
  border: 1px solid var(--border-strong);
  border-radius: 14px;
  background: var(--surface);
}

.form-section h2 {
  margin: 0 0 16px;
  font-size: 15px;
  font-weight: 650;
  letter-spacing: -0.01em;
  color: var(--text);
}

.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 14px;
}

.field:last-child {
  margin-bottom: 0;
}

.checkbox-field {
  flex-direction: row;
  align-items: center;
  gap: 10px;
}

.checkbox-field input[type='checkbox'] {
  width: 18px;
  height: 18px;
  margin: 0;
  flex-shrink: 0;
}

.field-help {
  margin-left: 22px;
  font-size: 11.5px;
  color: var(--text-faint);
}

.field label {
  font-size: 12.5px;
  font-weight: 600;
  letter-spacing: -0.005em;
  color: var(--text-muted);
}

.field input[type='text'],
.field input[type='number'],
.field select {
  padding: 9px 12px;
  font-size: 13.5px;
  color: var(--text);
  background: var(--surface-2);
  border: 1px solid var(--border-strong);
  border-radius: 8px;
  outline: none;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}

.field input:focus,
.field select:focus {
  border-color: var(--accent-strong);
  box-shadow: 0 0 0 3px rgba(62, 207, 172, 0.15);
}

.field input:disabled,
.field select:disabled {
  background: var(--surface-2);
  color: var(--text-faint);
  cursor: not-allowed;
}

.field select {
  appearance: none;
  background-image: url("data:image/svg+xml,%3csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none'%3e%3cpath d='M6 9l6 6 6-6' stroke='%23627d98' stroke-width='1.5' stroke-linecap='round' stroke-linejoin='round'/%3e%3c/svg%3e");
  background-repeat: no-repeat;
  background-position: right 10px center;
  background-size: 16px;
  padding-right: 38px;
}

/* ----- Input group (password + eye toggle) ---------------------------------- */
.input-group {
  position: relative;
  display: flex;
  align-items: center;
}

.input-group input {
  padding-right: 42px;
}

.toggle-eye {
  position: absolute;
  right: 6px;
  width: 28px;
  height: 28px;
  padding: 0;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--text-faint);
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  transition: color 0.15s ease, background 0.15s ease;
}

.toggle-eye:hover:not(:disabled) {
  color: var(--text-muted);
  background: var(--surface-2);
}

.toggle-eye:disabled {
  opacity: 0.4;
  cursor: not-allowed;
}

/* ----- Keyring banner ------------------------------------------------------- */
.keyring-banner {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  margin-bottom: 20px;
  border: 1px solid var(--accent-strong);
  border-radius: 10px;
  background: var(--accent-dim);
  color: var(--text);
  font-size: 13px;
  line-height: 1.5;
}

.keyring-glyph {
  width: 18px;
  height: 18px;
  color: var(--accent-strong);
  flex-shrink: 0;
}

/* ----- Loading skeleton ----------------------------------------------------- */
.loading-skeleton {
  display: flex;
  flex-direction: column;
  gap: 28px;
  pointer-events: none;
}

.skeleton-shimmer {
  height: 200px;
  border: 1px solid var(--border-strong);
  border-radius: 14px;
  background: var(--surface-2);
  animation: shimmer 1.5s ease-in-out infinite;
  background: linear-gradient(90deg, var(--surface-2) 0px, var(--surface) 50%, var(--surface-2) 100%);
  background-size: 200% 100%;
}

@keyframes shimmer {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}

@keyframes spin {
  to { transform: rotate(360deg); }
}

.spin {
  animation: spin 1s linear infinite;
}

/* ----- Inline error card (load error) --------------------------------------- */
.error-card {
  padding: 28px 32px;
  text-align: center;
  border: 1px solid var(--border-strong);
  border-radius: 16px;
  background: var(--surface);
}

.error-card h2 {
  margin: 0 0 8px;
  font-size: 16px;
  font-weight: 650;
  color: var(--text);
}

.error-card p {
  margin: 0 0 18px;
  font-size: 13px;
  color: var(--text-muted);
}

/* ----- Save error ----------------------------------------------------------- */
.save-error {
  padding: 10px 14px;
  font-size: 13px;
  font-weight: 500;
  color: var(--text);
  background: rgba(242, 118, 107, 0.12);
  border: 1px solid rgba(242, 118, 107, 0.3);
  border-radius: 8px;
}

/* ----- Buttons -------------------------------------------------------------- */
.form-actions {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
}

.btn-primary {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 10px 22px;
  font-size: 13.5px;
  font-weight: 600;
  color: var(--surface);
  background: var(--accent-strong);
  border: none;
  border-radius: 10px;
  cursor: pointer;
  transition: filter 0.15s ease;
}

.btn-primary:hover:not(:disabled) {
  filter: brightness(1.1);
}

.btn-primary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.btn-secondary {
  padding: 10px 22px;
  font-size: 13.5px;
  font-weight: 600;
  color: var(--text);
  background: transparent;
  border: 1px solid var(--border-strong);
  border-radius: 10px;
  cursor: pointer;
  transition: background 0.15s ease;
}

.btn-secondary:hover:not(:disabled) {
  background: var(--surface-2);
}

.btn-secondary:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}

.mono {
  font-family: var(--font-mono);
}
</style>
