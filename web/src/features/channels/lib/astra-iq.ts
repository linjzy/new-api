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
export type IQAttempt = {
  key_index: number
  status: string
  response?: string
  detail?: string
  error_message?: string
  error_code?: string
  error_type?: string
  latency_ms: number
  first_token_ms: number | null
  prompt_tokens: number | null
  completion_tokens: number | null
  http_status?: number
}
export type IQSample = {
  status: string
  checked_at: number
  started_at?: number
  answer?: string
  detail?: string
  latency_ms?: number
  endpoint?: string
  client?: string
  attempts?: IQAttempt[]
}
export type AstraIQStatusData = {
  enabled: boolean
  interval_minutes?: number
  server_time?: number
  results: Record<
    string,
    {
      status: string
      allowed: boolean
      pass_rate: number
      checked_at: number
      answer: string
      detail: string
      history: IQSample[]
    }
  >
}

// Keep real time gaps, including pauses and deleted results. A slow check
// belongs to the interval when it started, not the later completion interval.
export function astraIQTimeline(
  history: IQSample[],
  intervalMinutes: number,
  now: number
): (IQSample | null)[] {
  const interval = Math.max(1, intervalMinutes) * 60
  const firstSlot = Math.floor(now / interval) - 23
  const slots = Array<IQSample | null>(24).fill(null)
  for (const sample of history) {
    const started =
      sample.started_at ||
      sample.checked_at - Math.floor((sample.latency_ms || 0) / 1000)
    const index = Math.floor(started / interval) - firstSlot
    if (
      index >= 0 &&
      index < slots.length &&
      (!slots[index] || sample.checked_at > slots[index].checked_at)
    ) {
      slots[index] = sample
    }
  }
  return slots
}

export function astraIQStatusLabel(status: string) {
  if (status === 'pass') return 'Astra IQ passed'
  if (status === 'fail') return 'Astra IQ wrong answer'
  if (status === 'error') return 'Astra IQ request failed'
  if (status === 'expired') return 'Astra IQ expired'
  return 'Astra IQ pending'
}
