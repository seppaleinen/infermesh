import { onBeforeUnmount, ref } from 'vue'
import { RouterClient } from '../../bindings/github.com/seppaleinen/infermesh/app'
import type { PopularModelView } from '../../bindings/github.com/seppaleinen/infermesh/app/models'
import { useRouterPoll } from './useRouterPoll'

export function usePopularModels() {
	const { state, routerURL, start, stop, retry } = useRouterPoll<PopularModelView[]>(
		// @ts-expect-error RouterClient methods are bound via Wails
		(_) => RouterClient.GetPopularModels()
	)
	return { state, routerURL, start, stop, retry }
}