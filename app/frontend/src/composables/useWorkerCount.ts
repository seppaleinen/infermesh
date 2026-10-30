import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'
import { useRouterPoll } from './useRouterPoll'

export function useWorkerCount() {
	const { state, routerURL, start, stop, retry } = useRouterPoll<number>(
		(_) => RouterClient.GetConnectionCount()
	)
	return { state, routerURL, start, stop, retry }
}