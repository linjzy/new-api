/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import axios from 'axios'
import { useEffect } from 'react'
import { toast } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { CommonLogsStats } from '../components/common-logs-stats'
import {
  UsageLogsProvider,
  useUsageLogsContext,
} from '../components/usage-logs-provider'
import { getUsageLogsRefetchInterval } from '../lib/auto-refresh'

function Fixture() {
  const { autoRefresh, setAutoRefresh } = useUsageLogsContext()
  useEffect(() => {
    setAutoRefresh(true)
  }, [setAutoRefresh])
  return (
    <>
      <output aria-label='refresh state'>{String(autoRefresh)}</output>
      <CommonLogsStats />
    </>
  )
}

afterEach(() => {
  cleanup()
  localStorage.clear()
  vi.restoreAllMocks()
})

it.each(['business', 'http'])(
  'shows one toast and stops refresh on %s failure',
  async (kind) => {
    const toastSpy = vi.spyOn(toast, 'error')
    const previousAdapter = api.defaults.adapter
    let calls = 0
    api.defaults.adapter = async (config) => {
      calls++
      const response = {
        data: { success: false, message: 'test upstream unavailable' },
        status: kind === 'http' ? 503 : 200,
        statusText: 'Unavailable',
        headers: {},
        config,
      }
      if (kind === 'http') {
        throw new axios.AxiosError(
          'test upstream unavailable',
          'ERR_BAD_RESPONSE',
          config,
          undefined,
          response
        )
      }
      return response
    }
    const root = createRootRoute()
    const auth = createRoute({
      getParentRoute: () => root,
      id: '_authenticated',
    })
    const logs = createRoute({
      getParentRoute: () => auth,
      path: '/usage-logs/$section',
      component: () => (
        <UsageLogsProvider>
          <Fixture />
        </UsageLogsProvider>
      ),
      validateSearch: (search: Record<string, unknown>) => search,
    })
    const router = createRouter({
      routeTree: root.addChildren([auth.addChildren([logs])]),
      history: createMemoryHistory({ initialEntries: ['/usage-logs/common'] }),
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    try {
      render(
        <QueryClientProvider client={client}>
          <RouterProvider router={router} />
        </QueryClientProvider>
      )
      await waitFor(() => expect(toastSpy).toHaveBeenCalledTimes(1))
      expect(screen.getByLabelText('refresh state')).toHaveTextContent('false')
      expect(calls).toBe(1)
      expect(toastSpy.mock.calls[0]?.[0]).toContain('test upstream unavailable')
    } finally {
      api.defaults.adapter = previousAdapter
      client.clear()
    }
  }
)

it('refreshes only a successful first page when enabled', () => {
  expect(getUsageLogsRefetchInterval(true, 1, 'success')).toBeGreaterThan(0)
  expect(getUsageLogsRefetchInterval(false, 1, 'success')).toBe(false)
  expect(getUsageLogsRefetchInterval(true, 2, 'success')).toBe(false)
  expect(getUsageLogsRefetchInterval(true, 1, 'error')).toBe(false)
})
