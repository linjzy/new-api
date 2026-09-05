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
import { describe, expect, test } from 'vitest'

import { USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS } from '../../constants'
import { getUsageLogsRefetchInterval } from '../auto-refresh'

describe('usage logs auto refresh interval', () => {
  test('polls only while page one has a non-error query', () => {
    expect(getUsageLogsRefetchInterval(true, 1, 'success')).toBe(
      USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS
    )
    expect(getUsageLogsRefetchInterval(true, 1, 'pending')).toBe(
      USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS
    )
    expect(getUsageLogsRefetchInterval(true, 1, 'error')).toBe(false)
    expect(getUsageLogsRefetchInterval(true, 2, 'success')).toBe(false)
    expect(getUsageLogsRefetchInterval(false, 1, 'success')).toBe(false)
  })
})
