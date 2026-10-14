import { computed, ref } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'

// useRouterAuth exposes the desktop's router authentication status to the UI.
// The Supervisor manages the actual key sync (issue #119); this composable
// only reads the current value so the UI can display it. An empty key means
// no auth header is sent (dev mode); a non-empty key means the desktop is
// authenticating the router with X-API-Key.
//
// Fetched once on mount — the key only changes on router start/stop, so a
// single read is sufficient. Callers may refresh by calling fetch().
export function useRouterAuth() {
	const apiKey = ref('')
	const loaded = ref(false)

	async function fetch(): Promise<void> {
		try {
			apiKey.value = await RouterClient.GetAPIKey()
		} catch {
			// Router unreachable — treat as no key (dev-mode fallback).
			apiKey.value = ''
		}
		loaded.value = true
	}

	// Short masked display: "authenticated" when a key is set, "no key" when
	// empty. The full key value is never shown (it is a secret).
	const label = computed(() => (apiKey.value ? 'Authenticated' : 'No API key'))

	return { apiKey, loaded, fetch, label }
}