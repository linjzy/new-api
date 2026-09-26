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
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import {
  astraIQStatusLabel,
  astraIQTimeline,
  type AstraIQStatusData,
  type IQSample,
} from '../lib/astra-iq'
import { AstraIQDetailDialog } from './dialogs/astra-iq-detail-dialog'

export type { AstraIQStatusData } from '../lib/astra-iq'

async function getAstraIQStatus(
  signal: AbortSignal
): Promise<AstraIQStatusData> {
  const response = await api.get<{ success: boolean; data: AstraIQStatusData }>(
    '/api/channel/astra_iq',
    { signal }
  )
  return requireServerSuccess(response.data).data
}

// Individual checks form a discrete timeline; Progress cannot represent or
// activate separate historical outcomes. Buttons and tooltips use shared UI.
export function AstraIQStatus(props: {
  channelId: number
  channelEnabled?: boolean
}) {
  const { t, i18n } = useTranslation()
  const [selected, setSelected] = useState<IQSample | null>(null)
  const query = useQuery({
    queryKey: ['channels', 'astra-iq'],
    queryFn: ({ signal }) => getAstraIQStatus(signal),
    staleTime: 15_000,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    meta: { errorToast: false },
  })
  if (query.data?.enabled === false) return null
  const result = query.data?.results[String(props.channelId)]
  const history = query.isError ? [] : (result?.history?.slice(-24) ?? [])
  const samples = astraIQTimeline(
    history,
    query.data?.interval_minutes ?? 5,
    query.data?.server_time ?? Date.now() / 1000,
    query.data?.runs
  )
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  let state = t(astraIQStatusLabel(result?.status ?? 'pending'))
  if (props.channelEnabled === false) state = t('Astra IQ disabled channel')
  if (query.isError) state = t('Astra IQ status unavailable')

  return (
    <>
      <div
        className='flex h-5 min-w-0 flex-1 items-center gap-0.5'
        role='group'
        data-slot='astra-iq-timeline'
        aria-label={`${t('Astra IQ')}: ${state}`}
      >
        {samples.map((sample, index) => {
          if (!sample) {
            return (
              <span
                key={`empty-${24 - index}`}
                aria-hidden='true'
                className='h-3.5 min-w-0 flex-1'
              />
            )
          }
          const label = `${t(astraIQStatusLabel(sample.status))} · ${new Date(sample.checked_at * 1000).toLocaleString(locale)}`
          return (
            <Tooltip key={sample.checked_at}>
              <TooltipTrigger
                render={
                  <Button
                    variant='ghost'
                    aria-label={label}
                    onClick={() => setSelected(sample)}
                    className='group h-5 min-w-0 flex-1 rounded-md border-0 p-0 hover:bg-transparent focus-visible:ring-2'
                  />
                }
              >
                <span
                  className={cn(
                    'h-3.5 w-full rounded-[3px] ring-1 ring-inset ring-black/5 transition-[height,filter,box-shadow] duration-150 group-hover:h-5 group-hover:brightness-110 group-focus-visible:h-5',
                    sample.status === 'pass' &&
                      'bg-emerald-500 shadow-[inset_0_1px_0_#ffffff40,0_1px_2px_#10b98125]',
                    sample.status === 'fail' &&
                      'bg-rose-500 shadow-[inset_0_1px_0_#ffffff40,0_1px_2px_#f43f5e25]',
                    sample.status === 'error' &&
                      'bg-amber-400 shadow-[inset_0_1px_0_#ffffff50,0_1px_2px_#fbbf2425]'
                  )}
                />
              </TooltipTrigger>
              <TooltipContent className='space-y-1 text-center'>
                <p>{label}</p>
                <p className='opacity-70'>{t('View check details')}</p>
              </TooltipContent>
            </Tooltip>
          )
        })}
      </div>
      {selected && (
        <AstraIQDetailDialog
          channelId={props.channelId}
          sample={selected}
          onClose={() => setSelected(null)}
        />
      )}
    </>
  )
}
