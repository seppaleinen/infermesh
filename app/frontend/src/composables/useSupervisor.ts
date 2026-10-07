import { onBeforeUnmount, ref } from 'vue'
import { Supervisor } from '../../bindings/github.com/seppaleinen/infermesh/app'

export function useSupervisor() {
  const status = ref<any>(null)
  let timer: ReturnType<typeof setInterval> | undefined

  async function fetch() {
    try { status.value = await Supervisor.Status() } catch { status.value = null }
  }

  fetch()
  timer = setInterval(fetch, 5000)
  onBeforeUnmount(() => { if (timer) clearInterval(timer) })
  return status
}
