import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '@bindings/github.com/seppaleinen/infermesh/app'
import type { InflightRequestView } from '@bindings/github.com/seppaleinen/infermesh/app/models'
import { useRouterPoll } from './useRouterPoll'

export function useInflightRequests() {
	const { state, routerURL, start, stop, retry } = useRouterPoll<InflightRequestView[]>(
		(_) => RouterClient.GetInflightRequests()
	)
	return { state, routerURL, start, stop, retry }
}