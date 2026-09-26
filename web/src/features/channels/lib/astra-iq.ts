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
  runs?: IQRun[]
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

type IQRun = { started_at: number; finished_at: number }

// Each actual scheduler round occupies one position. Account for the whole
// round's duration and the scheduler/runner's two 15-second polling windows;
// wall-clock minute boundaries must not turn normal delays into missed checks.
export function astraIQTimeline(
  history: IQSample[],
  intervalMinutes: number,
  now: number,
  runs: IQRun[] = []
): (IQSample | null)[] {
  const interval = Math.max(1, intervalMinutes) * 60
  const rounds = [...runs]
  const samples = new Map<IQRun, IQSample>()
  for (const sample of history) {
    const started =
      sample.started_at ||
      sample.checked_at - Math.floor((sample.latency_ms || 0) / 1000)
    let round: IQRun | undefined
    for (let i = rounds.length - 1; i >= 0; i--) {
      const candidate = rounds[i]
      if (
        started >= candidate.started_at - 1 &&
        started <= (candidate.finished_at || now) + 1
      ) {
        round = candidate
        break
      }
    }
    // Task history can be cleared independently of saved check results.
    if (!round) {
      round = { started_at: started, finished_at: sample.checked_at }
      rounds.push(round)
    }
    const previous = samples.get(round)
    if (!previous || sample.checked_at > previous.checked_at) {
      samples.set(round, sample)
    }
  }
  rounds.sort((a, b) => a.started_at - b.started_at)
  const slots: (IQSample | null)[] = []
  let dueAt: number | undefined
  for (const round of rounds) {
    if (dueAt !== undefined) {
      const missed = Math.min(
        24,
        Math.max(0, Math.ceil((round.started_at - dueAt - 30) / interval))
      )
      slots.push(...Array<IQSample | null>(missed).fill(null))
    }
    slots.push(samples.get(round) ?? null)
    dueAt = Math.max(round.started_at + interval, round.finished_at || now)
  }
  if (dueAt !== undefined) {
    const missed = Math.min(
      24,
      Math.max(0, Math.ceil((now - dueAt - 30) / interval))
    )
    slots.push(...Array<IQSample | null>(missed).fill(null))
  }
  return [...Array<IQSample | null>(24).fill(null), ...slots].slice(-24)
}

export function astraIQStatusLabel(status: string) {
  if (status === 'pass') return 'Astra IQ passed'
  if (status === 'fail') return 'Astra IQ wrong answer'
  if (status === 'error') return 'Astra IQ request failed'
  if (status === 'expired') return 'Astra IQ expired'
  return 'Astra IQ pending'
}
