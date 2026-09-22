import { ref } from 'vue'
import { ConfigService, type Settings } from '../../bindings/github.com/seppaleinen/infermesh/app'

/**
 * Secret reference mapping from config_service.go knownSecretRefs.
 * Each ref corresponds to a field on Settings; the value is stored in the
 * OS keyring, never in the YAML file.
 */
const SECRET_REFS = {
  'router/apikey': 'router_apikey',
  'worker/customauth': 'worker_custom_auth',
  'mtls/cert': 'worker_mtls_cert',
  'mtls/key': 'worker_mtls_key',
} as const

export type SecretRefKey = keyof typeof SECRET_REFS

export function emptySettings(): Settings {
  return {
    router_addr: '',
    router_apikey: '',
    router_binary_path: '',
    worker_addr: '',
    worker_apikey: '',
    worker_backend: 'llama-cpp',
    worker_model_path: '',
    worker_port: 0,
    worker_binary_path: '',
    worker_custom_auth: '',
    worker_mtls_cert: '',
    worker_mtls_key: '',
    relay_url: '',
    dev_mode: null,
    worker_enable_health_checks: null,
    auto_start_on_login: null,
    secrets: {},
    secret_refs: {},
  }
}

export function secretRefsForForm(): string[] {
  return Object.keys(SECRET_REFS) as SecretRefKey[]
}

export function useSettings() {
  const settings = ref<Settings | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)
  const isKeyringAvailable = ref(false)

  /**
   * Load settings from disk.
   * Returns the loaded Settings object, or throws on failure.
   */
  async function load(): Promise<Settings> {
    loading.value = true
    error.value = null
    try {
      const s = await ConfigService.LoadSettings()
      settings.value = s
      isKeyringAvailable.value = await ConfigService.IsKeyringAvailable()
      return s
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      error.value = msg
      settings.value = null
      throw e
    } finally {
      loading.value = false
    }
  }

/**
    * Save settings to disk.
    * Returns true on success, false on keyring-unavailable error.
    *
    * NOTE: SaveSettings(s) on the Go side ALREADY writes every non-empty
    * s.secrets entry to the keyring (cs.kr.Set) and merges SecretRefs into
    * the YAML. The JS layer must NOT call SetSecret afterwards — SetSecret
    * reloads the YAML from disk and rewrites it, which would clobber the
    * non-secret fields SaveSettings just persisted. The only thing the UI
    * does here is pass the assembled secrets map and clear its local copy.
    */
  async function save(s: Settings): Promise<boolean> {
    error.value = null
    try {
      const keyringAvailable = await ConfigService.SaveSettings(s)
      isKeyringAvailable.value = keyringAvailable
      if (!keyringAvailable) return false

      // Clear local secret refs after success (the values are already in
      // the keyring; the YAML only ever holds references).
      if (s.secrets) {
        for (const ref of Object.keys(s.secrets)) {
          s.secrets[ref] = ''
        }
      }

      return true
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      error.value = msg
      throw e
    }
  }

  /**
   * Set a secret value in the keyring.
   */
  async function setSecret(ref: string, value: string): Promise<void> {
    try {
      await ConfigService.SetSecret(ref, value)
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      error.value = msg
      throw e
    }
  }

/**
    * Get a secret value from the keyring.
    * Returns '' if the secret does not exist OR the keyring is unavailable,
    * so the UI renders a blank field rather than an error card. Both cases
    * are "no secret to show" from the user's perspective — the keyring-down
    * state is already surfaced by the banner and disabled inputs, and
    * polluting the load error ref here would prevent the form from
    * rendering at all on a keyring-less host.
    */
  async function getSecret(ref: string): Promise<string> {
    try {
      return await ConfigService.GetSecret(ref)
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      if (
        msg.includes('secret not found') ||
        msg.includes('keyring is unavailable')
      ) {
        return ''
      }
      error.value = msg
      throw e
    }
  }

  /**
   * Delete a secret from the keyring.
   */
  async function deleteSecret(ref: string): Promise<void> {
    try {
      await ConfigService.DeleteSecret(ref)
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      error.value = msg
      throw e
    }
  }

  /**
   * Refresh keyring availability.
   * Returns true if the keyring is available.
   */
  async function refreshKeyring(): Promise<boolean> {
    const available = await ConfigService.IsKeyringAvailable()
    isKeyringAvailable.value = available
    return available
  }

  return {
    settings,
    loading,
    error,
    isKeyringAvailable,
    load,
    save,
    setSecret,
    getSecret,
    deleteSecret,
    refreshKeyring,
    emptySettings,
    secretRefsForForm,
  }
}