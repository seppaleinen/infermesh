import { onBeforeUnmount, ref } from 'vue'
import { Supervisor } from '@bindings/github.com/seppaleinen/infermesh/app'
import type { SupervisorStatus } from '@bindings/github.com/seppaleinen/infermesh/app'

export function useSupervisor() {
  const status = ref<SupervisorStatus | null>(null)
  let timer: ReturnType<typeof setInterval> | undefined

  async function fetch() {
    try { status.value = await Supervisor.Status() } catch { status.value = null }
  }

  async function startWorker() {
    await Supervisor.StartWorker()
    await fetch()
  }

  async function stopWorker() {
    await Supervisor.StopWorker()
    await fetch()
  }

  async function restartWorker() {
    await Supervisor.RestartWorker()
    await fetch()
  }

  async function startRouter() {
    await Supervisor.StartRouter()
    await fetch()
  }

  async function stopRouter() {
    await Supervisor.StopRouter()
    await fetch()
  }

  async function restartRouter() {
    await Supervisor.RestartRouter()
    await fetch()
  }

  fetch()
  timer = setInterval(fetch, 5000)
  onBeforeUnmount(() => { if (timer) clearInterval(timer) })
  return { status, startWorker, stopWorker, restartWorker, startRouter, stopRouter, restartRouter }
}
