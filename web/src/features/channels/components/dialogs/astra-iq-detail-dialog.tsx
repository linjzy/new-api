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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { markServerErrorHandled } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import {
  astraIQStatusLabel,
  type IQAttempt,
  type IQSample,
} from '../../lib/astra-iq'

export type IQCheckDetail = {
  sample: IQSample
  question: string
  expected_answer: string
  model: string
  reasoning_effort: string
}

type Props = { channelId: number; sample: IQSample; onClose: () => void }

function CheckFailure(props: {
  attempt: Partial<IQAttempt>
  expected: string
}) {
  const { t } = useTranslation()
  const attempt = props.attempt
  if (attempt.status !== 'fail' && attempt.status !== 'error') return null
  let message = attempt.error_message
  if (attempt.status === 'fail') {
    message = t('Expected {{expected}}, received {{actual}}.', {
      expected: props.expected,
      actual: attempt.response || '—',
    })
  } else if (!message) {
    const reasons: Record<string, string> = {
      timeout: t('The check exceeded its time limit.'),
      official_client_rejected: t(
        'Upstream rejected the official Codex client.'
      ),
      official_client_unavailable: t(
        'The official Codex client could not be started.'
      ),
      official_client_failed: t(
        'The official Codex client did not send a request.'
      ),
      official_client_incomplete: t(
        'The official Codex client did not receive a complete response.'
      ),
      empty_answer: t('The upstream response contained no final answer.'),
      incomplete_response: t('The upstream response ended before completion.'),
      no_enabled_keys: t('This channel has no enabled keys.'),
    }
    message =
      reasons[attempt.detail || ''] ||
      t('The request failed. This older check has no saved error details.')
  }
  return (
    <Alert variant='destructive' className='space-y-2 p-4'>
      <AlertTitle>{t('Failure reason')}</AlertTitle>
      <AlertDescription className='space-y-2'>
        <p className='break-words whitespace-pre-wrap'>{message}</p>
        <div className='flex flex-wrap gap-x-4 gap-y-1 font-mono text-xs break-all'>
          {attempt.http_status ? <span>HTTP {attempt.http_status}</span> : null}
          {attempt.error_code && (
            <span>
              {t('Error code')}: {attempt.error_code}
            </span>
          )}
          {attempt.error_type && (
            <span>
              {t('Error type')}: {attempt.error_type}
            </span>
          )}
          {!attempt.error_code && attempt.detail && (
            <span>
              {t('Error code')}: {attempt.detail}
            </span>
          )}
        </div>
      </AlertDescription>
    </Alert>
  )
}

export function AstraIQDetailDialog(props: Props) {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const currentUser = useAuthStore((state) => state.auth.user)
  const canDelete = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const [deleteScope, setDeleteScope] = useState<'this' | 'all' | null>(null)
  const deletion = useMutation({
    mutationFn: async (scope: 'this' | 'all') => {
      const endpoint = `/api/channel/astra_iq/${props.channelId}`
      const response = await api.delete<{ success: boolean; message?: string }>(
        scope === 'all' ? endpoint : `${endpoint}/${props.sample.checked_at}`
      )
      requireServerSuccess(response.data)
    },
    onSuccess: (_, scope) => {
      setDeleteScope(null)
      props.onClose()
      queryClient.removeQueries({
        queryKey:
          scope === 'all'
            ? ['channels', 'astra-iq-detail', props.channelId]
            : [
                'channels',
                'astra-iq-detail',
                props.channelId,
                props.sample.checked_at,
              ],
      })
      void queryClient.invalidateQueries({ queryKey: ['channels', 'astra-iq'] })
    },
    onError: markServerErrorHandled,
    meta: { errorToast: false },
  })
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const query = useQuery({
    queryKey: [
      'channels',
      'astra-iq-detail',
      props.channelId,
      props.sample.checked_at,
    ],
    queryFn: async ({ signal }) => {
      const response = await api.get<{ success: boolean; data: IQCheckDetail }>(
        `/api/channel/astra_iq/${props.channelId}/${props.sample.checked_at}`,
        { signal }
      )
      return requireServerSuccess(response.data).data
    },
    staleTime: Infinity,
    meta: { errorToast: false },
  })
  const detail = query.data
  const sample = detail?.sample ?? props.sample
  const attempts = sample.attempts ?? []
  const hasUsage =
    attempts.length > 0 &&
    attempts.every(
      (attempt) =>
        attempt.prompt_tokens != null && attempt.completion_tokens != null
    )
  const inputTokens = attempts.reduce(
    (total, attempt) => total + (attempt.prompt_tokens ?? 0),
    0
  )
  const outputTokens = attempts.reduce(
    (total, attempt) => total + (attempt.completion_tokens ?? 0),
    0
  )
  const firstToken = attempts[0]?.first_token_ms
  const metrics = [
    {
      label: t('Latency'),
      value:
        sample.latency_ms == null
          ? '—'
          : `${formatNumber(sample.latency_ms / 1000, locale)} s`,
    },
    {
      label: t('Time to first token'),
      value:
        firstToken == null
          ? '—'
          : `${formatNumber(firstToken / 1000, locale)} s`,
    },
    {
      label: t('Input / Output tokens'),
      value: hasUsage
        ? `${formatNumber(inputTokens, locale)} / ${formatNumber(outputTokens, locale)}`
        : '—',
    },
    {
      label: t('Check requests'),
      value: attempts.length ? formatNumber(attempts.length, locale) : '—',
    },
  ]

  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open && !deletion.isPending) props.onClose()
        }}
        title={t('Astra IQ check details')}
        description={new Date(sample.checked_at * 1000).toLocaleString(locale)}
        contentClassName='rounded-2xl sm:max-w-3xl'
        headerClassName='border-b border-border/60 pb-4 pr-6'
        bodyClassName='space-y-6'
        footer={
          canDelete && (
            <>
              <Button
                variant='outline'
                disabled={deletion.isPending}
                onClick={() => {
                  deletion.reset()
                  setDeleteScope('this')
                }}
              >
                {t('Delete this check')}
              </Button>
              <Button
                variant='destructive'
                disabled={deletion.isPending}
                onClick={() => {
                  deletion.reset()
                  setDeleteScope('all')
                }}
              >
                {t('Delete all checks')}
              </Button>
            </>
          )
        }
      >
        {query.isPending && <LoadingState />}
        {query.isError && (
          <ErrorState
            title={t('Astra IQ details unavailable')}
            onRetry={() => void query.refetch()}
          />
        )}
        {detail && !query.isError && (
          <>
            <div className='flex flex-wrap items-center justify-between gap-3'>
              <div className='flex flex-wrap items-center gap-3'>
                <span
                  className={cn(
                    'inline-flex items-center gap-2 rounded-full px-3 py-1.5 text-xs font-medium ring-1 ring-inset',
                    sample.status === 'pass' &&
                      'bg-emerald-500/8 text-emerald-700 ring-emerald-500/20 dark:text-emerald-400',
                    sample.status === 'fail' &&
                      'bg-rose-500/8 text-rose-700 ring-rose-500/20 dark:text-rose-400',
                    sample.status === 'error' &&
                      'bg-amber-500/8 text-amber-700 ring-amber-500/20 dark:text-amber-400'
                  )}
                >
                  <span className='size-1.5 rounded-full bg-current' />
                  {t(astraIQStatusLabel(sample.status))}
                </span>
                <span className='text-muted-foreground text-sm'>
                  {t('Expected answer')}:{' '}
                  <strong className='text-foreground font-medium'>
                    {detail.expected_answer}
                  </strong>
                </span>
              </div>
              <div className='text-muted-foreground flex flex-wrap items-center gap-2 font-mono text-xs'>
                <span>{detail.model}</span>
                <span aria-hidden='true'>·</span>
                <span>{detail.reasoning_effort}</span>
                {sample.client && (
                  <>
                    <span aria-hidden='true'>·</span>
                    <span>{sample.client}</span>
                  </>
                )}
              </div>
            </div>
            <CheckFailure
              attempt={
                attempts.find(
                  (attempt) =>
                    attempt.status === 'fail' || attempt.status === 'error'
                ) || {
                  status: sample.status,
                  detail: sample.detail,
                  response: sample.answer,
                }
              }
              expected={detail.expected_answer}
            />
            <section className='space-y-2.5'>
              <div className='flex flex-wrap items-center justify-between gap-2'>
                <h3 className='text-sm font-medium'>
                  {t('Question sent to model')}
                </h3>
                {sample.endpoint && (
                  <span className='text-muted-foreground font-mono text-xs'>
                    {sample.endpoint}
                  </span>
                )}
              </div>
              <div className='bg-muted/35 border-border/60 rounded-xl border p-4 text-sm leading-7 break-words whitespace-pre-wrap'>
                {detail.question}
              </div>
            </section>
            <dl className='bg-muted/35 grid grid-cols-2 gap-x-5 gap-y-4 rounded-xl p-4 sm:grid-cols-4'>
              {metrics.map((metric) => (
                <div key={metric.label} className='space-y-1.5'>
                  <dt className='text-muted-foreground text-xs'>
                    {metric.label}
                  </dt>
                  <dd className='font-mono text-sm font-medium tabular-nums'>
                    {metric.value}
                  </dd>
                </div>
              ))}
            </dl>
            <section className='space-y-3'>
              <h3 className='text-sm font-medium'>{t('Model response')}</h3>
              {!attempts.length && (
                <div className='border-border/60 rounded-xl border p-4 text-sm'>
                  {sample.answer && (
                    <p className='mb-2 whitespace-pre-wrap'>{sample.answer}</p>
                  )}
                  <p className='text-muted-foreground'>
                    {t('Detailed metrics were not saved for this older check.')}
                  </p>
                </div>
              )}
              {attempts.map((attempt, index) => (
                <div
                  key={attempt.key_index}
                  className='border-border/60 space-y-3 rounded-xl border p-4'
                >
                  {attempts.length > 1 && (
                    <p className='text-muted-foreground text-xs'>
                      {t('Check request')} {formatNumber(index + 1, locale)} ·{' '}
                      {t(astraIQStatusLabel(attempt.status))}
                    </p>
                  )}
                  {attempt.response ? (
                    <p className='text-sm leading-7 break-words whitespace-pre-wrap'>
                      {attempt.response}
                    </p>
                  ) : (
                    <p className='text-muted-foreground text-sm'>
                      {t('No completed model response')}
                    </p>
                  )}
                </div>
              ))}
            </section>
          </>
        )}
      </Dialog>
      <ConfirmDialog
        open={deleteScope !== null}
        onOpenChange={(open) => {
          if (!open && !deletion.isPending) setDeleteScope(null)
        }}
        title={
          deleteScope === 'all'
            ? t('Delete all checks for this channel?')
            : t('Delete this check?')
        }
        desc={
          deleteScope === 'all'
            ? t(
                'This permanently deletes all check results for this channel. Calls are allowed until the next check.'
              )
            : t(
                'This permanently deletes this check. If it is the latest result, calls are allowed until the next check.'
              )
        }
        confirmText={t('Delete')}
        destructive
        isLoading={deletion.isPending}
        handleConfirm={() => {
          if (deleteScope && canDelete && !deletion.isPending) {
            deletion.mutate(deleteScope)
          }
        }}
      >
        {deletion.isError && (
          <p role='alert' className='text-destructive text-sm'>
            {getServerErrorMessage(
              deletion.error,
              t('Failed to delete check results')
            )}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}
