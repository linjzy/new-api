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
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { astraIQTimeline } from '../../lib/astra-iq'
import { AstraIQSettings } from '../astra-iq-settings'
import { AstraIQStatus, type AstraIQStatusData } from '../astra-iq-status'
import type { IQCheckDetail } from '../dialogs/astra-iq-detail-dialog'

const originalAuth = useAuthStore.getState().auth
beforeEach(() => {
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
})
afterEach(() => {
  vi.restoreAllMocks()
  useAuthStore.setState({ auth: originalAuth })
})

const history = [
  { status: 'fail', checked_at: 100, answer: '22' },
  { status: 'pass', checked_at: 200, answer: '21' },
]
const statusData: AstraIQStatusData = {
  enabled: true,
  interval_minutes: 1,
  server_time: 200,
  results: {
    '2': {
      status: 'pass',
      allowed: true,
      pass_rate: 50,
      checked_at: 200,
      answer: '21',
      detail: '',
      history,
    },
  },
}
const detail: IQCheckDetail = {
  question: 'The exact candy question sent upstream.',
  expected_answer: '21',
  model: 'gpt-6-astra',
  reasoning_effort: 'medium',
  sample: {
    status: 'fail',
    checked_at: 100,
    answer: '22',
    latency_ms: 30100,
    endpoint: '/v1/responses',
    client: 'Codex CLI 0.157.1',
    attempts: [
      {
        key_index: 1,
        status: 'fail',
        response: '22',
        latency_ms: 30100,
        first_token_ms: 19000,
        prompt_tokens: 4631,
        completion_tokens: 1540,
        detail: 'wrong_answer',
        http_status: 200,
      },
    ],
  },
}

function renderStatus() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client.setQueryData(['channels', 'astra-iq'], statusData)
  render(
    <QueryClientProvider client={client}>
      <AstraIQStatus channelId={2} />
    </QueryClientProvider>
  )
  return client
}

it('opens the selected historical check rather than the newest result', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: detail } })
  const client = renderStatus()
  await userEvent.click(
    screen.getByRole('button', { name: /Astra IQ wrong answer/ })
  )
  const dialog = await screen.findByRole('dialog')
  await waitFor(() =>
    expect(within(dialog).getByText(detail.question)).toBeVisible()
  )
  expect(get).toHaveBeenCalledWith(
    '/api/channel/astra_iq/2/100',
    expect.objectContaining({ signal: expect.any(AbortSignal) })
  )
  expect(within(dialog).getByText('22')).toBeVisible()
  expect(within(dialog).getByRole('alert')).toHaveTextContent(
    'Expected 21, received 22.'
  )
  expect(within(dialog).getByText('medium')).toBeVisible()
  expect(within(dialog).getByText('/v1/responses')).toBeVisible()
  expect(within(dialog).getByText('Codex CLI 0.157.1')).toBeVisible()
  expect(within(dialog).getByText('19 s')).toBeVisible()
  expect(within(dialog).getByText('4,631 / 1,540')).toBeVisible()
  client.setQueryData(['channels', 'astra-iq'], { ...statusData, results: {} })
  expect(within(dialog).getByText('22')).toBeVisible()
})

it('does not keep displaying an old pass when status polling fails', async () => {
  vi.spyOn(api, 'get').mockRejectedValue(new Error('offline'))
  const client = renderStatus()
  await client.invalidateQueries({ queryKey: ['channels', 'astra-iq'] })
  expect(
    await screen.findByRole('group', { name: /Astra IQ status unavailable/ })
  ).toBeInTheDocument()
  expect(screen.queryByRole('button')).not.toBeInTheDocument()
})

it('allows retrying a failed detail request', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('offline'))
    .mockResolvedValue({ data: { success: true, data: detail } })
  renderStatus()
  await userEvent.click(
    screen.getByRole('button', { name: /Astra IQ wrong answer/ })
  )
  expect(await screen.findByText('Astra IQ details unavailable')).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText(detail.question)).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
})

it('keeps unavailable metrics blank for records created before detail capture', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        ...detail,
        sample: { status: 'fail', checked_at: 100, answer: '22' },
      },
    },
  })
  renderStatus()
  await userEvent.click(
    screen.getByRole('button', { name: /Astra IQ wrong answer/ })
  )
  expect(
    await screen.findByText(
      'Detailed metrics were not saved for this older check.'
    )
  ).toBeVisible()
  expect(screen.getAllByText('—')).toHaveLength(4)
})

it.each([
  ['Delete this check', '/api/channel/astra_iq/2/100'],
  ['Delete all checks', '/api/channel/astra_iq/2'],
])(
  'confirms %s and refreshes the timeline after deletion',
  async (label, endpoint) => {
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/channel/astra_iq'
            ? { enabled: true, results: {} }
            : detail,
      },
    }))
    let resolveDelete!: (value: unknown) => void
    const pending = new Promise<unknown>((resolve) => {
      resolveDelete = resolve
    })
    const remove = vi.spyOn(api, 'delete').mockReturnValue(pending as never)
    renderStatus()
    await userEvent.click(
      screen.getByRole('button', { name: /Astra IQ wrong answer/ })
    )
    await screen.findByText(detail.question)
    await userEvent.click(screen.getByRole('button', { name: label }))
    await userEvent.click(
      within(screen.getByRole('alertdialog')).getByRole('button', {
        name: 'Cancel',
      })
    )
    expect(remove).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: label }))
    const confirmation = screen.getByRole('alertdialog')
    await userEvent.click(
      within(confirmation).getByRole('button', { name: 'Delete' })
    )
    expect(remove).toHaveBeenCalledWith(endpoint)
    expect(
      within(confirmation).getByRole('button', { name: 'Delete' })
    ).toBeDisabled()
    resolveDelete({ data: { success: true } })
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    await waitFor(() =>
      expect(screen.queryByRole('button')).not.toBeInTheDocument()
    )
  }
)

it.each(['business', 'network'])(
  'keeps history and permits retry after a %s deletion error',
  async (kind) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: detail },
    })
    const remove = vi.spyOn(api, 'delete')
    if (kind === 'business') {
      remove.mockResolvedValue({
        data: { success: false, message: 'Cannot delete this result' },
      })
    } else {
      remove.mockRejectedValue(new Error('Cannot delete this result'))
    }
    renderStatus()
    await userEvent.click(
      screen.getByRole('button', { name: /Astra IQ wrong answer/ })
    )
    await screen.findByText(detail.question)
    await userEvent.click(
      screen.getByRole('button', { name: 'Delete this check' })
    )
    const confirmation = screen.getByRole('alertdialog')
    await userEvent.click(
      within(confirmation).getByRole('button', { name: 'Delete' })
    )
    expect(await within(confirmation).findByRole('alert')).toHaveTextContent(
      'Cannot delete this result'
    )
    expect(
      within(confirmation).getByRole('button', { name: 'Delete' })
    ).toBeEnabled()
    await userEvent.click(
      within(confirmation).getByRole('button', { name: 'Cancel' })
    )
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(
      within(screen.getByRole('group')).getAllByRole('button')
    ).toHaveLength(2)
  }
)

it('does not offer deletion to a user with read-only channel access', async () => {
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: {
        id: 2,
        username: 'reader',
        role: ROLE.ADMIN,
        permissions: { admin_permissions: { channel: { read: true } } },
      },
    },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: detail },
  })
  renderStatus()
  await userEvent.click(
    screen.getByRole('button', { name: /Astra IQ wrong answer/ })
  )
  await screen.findByText(detail.question)
  expect(
    screen.queryByRole('button', { name: 'Delete this check' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Delete all checks' })
  ).not.toBeInTheDocument()
})

const defaultSettings = {
  enabled: true,
  start_time: '00:00',
  end_time: '00:00',
  interval_minutes: 5,
  stop_on_failure: true,
}

async function openSettings() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <AstraIQSettings />
    </QueryClientProvider>
  )
  await userEvent.click(screen.getByRole('button', { name: 'Check settings' }))
  return client
}

it('saves the total switch and schedule and reloads their values on reopen', async () => {
  let saved = { ...defaultSettings }
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation(async () => ({ data: { success: true, data: saved } }))
  const put = vi.spyOn(api, 'put').mockImplementation(async (_url, body) => {
    saved = body as typeof defaultSettings
    return { data: { success: true, data: saved } }
  })
  const client = await openSettings()
  const invalidate = vi.spyOn(client, 'invalidateQueries')
  fireEvent.change(await screen.findByLabelText('Check start time'), {
    target: { value: '22:00' },
  })
  fireEvent.change(screen.getByLabelText('End Time'), {
    target: { value: '06:30' },
  })
  fireEvent.change(screen.getByLabelText('Interval (minutes)'), {
    target: { value: '17' },
  })
  await userEvent.click(screen.getByRole('switch', { name: 'Stop on failure' }))
  await userEvent.click(screen.getByRole('switch', { name: 'Enable checks' }))
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(put).toHaveBeenCalledWith('/api/channel/astra_iq/settings', {
    enabled: false,
    start_time: '22:00',
    end_time: '06:30',
    interval_minutes: 17,
    stop_on_failure: false,
  })
  expect(invalidate).toHaveBeenCalledWith({
    queryKey: ['channels', 'astra-iq'],
  })
  await userEvent.click(screen.getByRole('button', { name: 'Check settings' }))
  expect(await screen.findByLabelText('Check start time')).toHaveValue('22:00')
  expect(screen.getByLabelText('End Time')).toHaveValue('06:30')
  expect(screen.getByLabelText('Interval (minutes)')).toHaveValue(17)
  expect(
    screen.getByRole('switch', { name: 'Stop on failure' })
  ).not.toBeChecked()
  expect(
    screen.getByRole('switch', { name: 'Enable checks' })
  ).not.toBeChecked()
  expect(get).toHaveBeenCalledTimes(2)
})

it('rejects a fractional interval before sending a request', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: defaultSettings },
  })
  const put = vi.spyOn(api, 'put')
  await openSettings()
  fireEvent.change(await screen.findByLabelText('Interval (minutes)'), {
    target: { value: '1.5' },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByText('Check interval must be an integer from 1 to 1440')
  ).toBeVisible()
  expect(put).not.toHaveBeenCalled()
})

it('disables saving after a load failure and allows retry', async () => {
  vi.spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('offline'))
    .mockResolvedValue({ data: { success: true, data: defaultSettings } })
  await openSettings()
  expect(await screen.findByText('Check settings unavailable')).toBeVisible()
  expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByLabelText('Interval (minutes)')).toHaveValue(5)
  expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
})

it('preserves edits after a save failure and permits retry', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: defaultSettings },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Cannot save check settings' },
    })
    .mockResolvedValue({ data: { success: true, data: defaultSettings } })
  await openSettings()
  fireEvent.change(await screen.findByLabelText('Interval (minutes)'), {
    target: { value: '20' },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Cannot save check settings'
  )
  expect(screen.getByLabelText('Interval (minutes)')).toHaveValue(20)
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(put).toHaveBeenCalledTimes(2)
})

it('allows channel readers to inspect settings without editing them', async () => {
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: {
        id: 2,
        username: 'reader',
        role: ROLE.ADMIN,
        permissions: { admin_permissions: { channel: { read: true } } },
      },
    },
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: defaultSettings },
  })
  await openSettings()
  expect(await screen.findByLabelText('Interval (minutes)')).toBeDisabled()
  expect(screen.getByLabelText('Check start time')).toBeDisabled()
  expect(
    screen.getByRole('switch', { name: 'Stop on failure' })
  ).toHaveAttribute('aria-disabled', 'true')
  await userEvent.click(screen.getByRole('switch', { name: 'Stop on failure' }))
  expect(screen.getByRole('switch', { name: 'Stop on failure' })).toBeChecked()
  expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
})

it.each([true, false])(
  'shows saved error details or an honest legacy fallback (saved=%s)',
  async (saved) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          ...detail,
          sample: {
            status: 'error',
            checked_at: 100,
            detail: 'request_failed',
            attempts: [
              {
                key_index: 1,
                status: 'error',
                detail: 'request_failed',
                latency_ms: 1700,
                http_status: 200,
                ...(saved
                  ? {
                      error_message: 'The usage limit has been reached.',
                      error_code: 'usage_limit_reached',
                      error_type: 'rate_limit_error',
                    }
                  : {}),
              },
            ],
          },
        },
      },
    })
    renderStatus()
    await userEvent.click(
      screen.getByRole('button', { name: /Astra IQ wrong answer/ })
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('HTTP 200')
    if (saved) {
      expect(alert).toHaveTextContent('The usage limit has been reached.')
      expect(alert).toHaveTextContent('usage_limit_reached')
      expect(alert).toHaveTextContent('rate_limit_error')
    } else {
      expect(alert).toHaveTextContent(
        'This older check has no saved error details.'
      )
    }
  }
)

it('leaves skipped intervals blank and hides the timeline when checks are off', async () => {
  const client = renderStatus()
  const timeline = screen.getByRole('group')
  const buttons = within(timeline).getAllByRole('button')
  // A missing scheduled check keeps its position between the two results.
  expect(buttons[0].nextElementSibling?.tagName).toBe('SPAN')
  expect(buttons[0].nextElementSibling?.nextElementSibling).toBe(buttons[1])
  await act(async () =>
    client.setQueryData(['channels', 'astra-iq'], {
      ...statusData,
      enabled: false,
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('group')).not.toBeInTheDocument()
  )
  await act(async () =>
    client.setQueryData(['channels', 'astra-iq'], statusData)
  )
  expect(
    within(await screen.findByRole('group')).getAllByRole('button')
  ).toHaveLength(2)
})

it.each([
  {
    name: 'scheduler drift and a slow check on another channel',
    starts: [103, 178, 238, 373, 448],
    ends: [115, 198, 358, 384, 460],
    deleted: -1,
    expected: [113, 188, 248, 383, 458],
  },
  {
    name: 'a pause spanning two scheduled checks',
    starts: [103, 178, 373],
    ends: [115, 198, 384],
    deleted: -1,
    expected: [113, 188, null, null, 383],
  },
  {
    name: 'a deleted result between continuing checks',
    starts: [103, 178, 238],
    ends: [115, 198, 250],
    deleted: 1,
    expected: [113, null, 248],
  },
])(
  'keeps the correct gaps for $name',
  ({ starts, ends, deleted, expected }) => {
    const records = starts.flatMap((started, index) =>
      index === deleted
        ? []
        : [{ status: 'pass', checked_at: started + 10, latency_ms: 10_000 }]
    )
    const runs = starts.map((started, index) => ({
      started_at: started,
      finished_at: ends[index],
    }))
    const slots = astraIQTimeline(records, 1, ends.at(-1) ?? 0, runs)
    expect(slots.map((sample) => sample?.checked_at ?? null)).toEqual([
      ...Array(24 - expected.length).fill(null),
      ...expected,
    ])
  }
)
