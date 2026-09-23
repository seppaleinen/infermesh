// Pure helper so a future test runner can adopt it without refactor.
// DefaultSettings/mergeDefaults treat dev_mode === null as dev-mode ON,
// so mTLS is shown only when dev_mode is explicitly false.
export function isMTLSVisible(devMode: boolean | null): boolean {
  return devMode === false
}
