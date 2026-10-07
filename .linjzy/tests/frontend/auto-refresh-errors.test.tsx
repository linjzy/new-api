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
import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import axios from 'axios'
import { useEffect } from 'react'
import { toast } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import { Toaster } from '@/components/ui/sonner'
import { ThemeProvider } from '@/context/theme-provider'
import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'

import { CommonLogsStats } from '../components/common-logs-stats'
import {
  UsageLogsProvider,
  useUsageLogsContext,
} from '../components/usage-logs-provider'
import { UsageLogsTable } from '../components/usage-logs-table'
import { getUsageLogsRefetchInterval } from '../lib/auto-refresh'

function Fixture(props: { view: 'stats' | 'table' }) {
  const { autoRefresh, setAutoRefresh } = useUsageLogsContext()
  useEffect(() => {
    setAutoRefresh(true)
  }, [setAutoRefresh])
  return (
    <>
      <output aria-label='refresh state'>{String(autoRefresh)}</output>
      {props.view === 'stats' ? (
        <CommonLogsStats />
      ) : (
        <UsageLogsTable logCategory='common' />
      )}
    </>
  )
}

afterEach(() => {
  toast.dismiss()
  cleanup()
  localStorage.clear()
  vi.restoreAllMocks()
})

it.each([
  ['stats', 'business'],
  ['stats', 'http'],
  ['table', 'business'],
  ['table', 'http'],
] as const)(
  '%s shows one toast and stops refresh on %s failure',
  async (view, kind) => {
    const previousAdapter = api.defaults.adapter
    let calls = 0
    let releaseResponse!: () => void
    const responseReady = new Promise<void>((resolve) => {
      releaseResponse = resolve
    })
    api.defaults.adapter = async (config) => {
      if (!config.url?.startsWith('/api/log')) {
        return {
          data: { success: true, data: [] },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      }
      calls++
      await responseReady
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
          <Fixture view={view} />
        </UsageLogsProvider>
      ),
      validateSearch: (search: Record<string, unknown>) => search,
    })
    const router = createRouter({
      routeTree: root.addChildren([auth.addChildren([logs])]),
      history: createMemoryHistory({ initialEntries: ['/usage-logs/common'] }),
    })
    const client = createAppQueryClient()
    try {
      render(
        <ThemeProvider>
          <QueryClientProvider client={client}>
            <RouterProvider router={router} />
            <Toaster />
          </QueryClientProvider>
        </ThemeProvider>
      )
      await waitFor(() =>
        expect(screen.getByLabelText('refresh state')).toHaveTextContent('true')
      )
      releaseResponse()
      await waitFor(() =>
        expect(screen.getByLabelText('refresh state')).toHaveTextContent(
          'false'
        )
      )
      await waitFor(() => expect(client.isFetching()).toBe(0))
      const notifications = screen.getByRole('region', {
        name: 'Notifications alt+T',
      })
      expect(
        within(notifications).getAllByText('test upstream unavailable')
      ).toHaveLength(1)
      expect(screen.getByLabelText('refresh state')).toHaveTextContent('false')
      expect(calls).toBeGreaterThan(0)
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
