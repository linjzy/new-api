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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Settings2 } from 'lucide-react'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { safeNumberFieldProps } from '@/features/system-settings/utils/numeric-field'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { api } from '@/lib/api'
import { markServerErrorHandled } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

const timeSchema = z
  .string()
  .regex(/^([01]\d|2[0-3]):[0-5]\d$/, 'Use HH:mm for check times')
const settingsSchema = z.object({
  enabled: z.boolean(),
  start_time: timeSchema,
  end_time: timeSchema,
  interval_minutes: z
    .number()
    .int('Check interval must be an integer from 1 to 1440')
    .min(1, 'Check interval must be an integer from 1 to 1440')
    .max(1440, 'Check interval must be an integer from 1 to 1440'),
  stop_on_failure: z.boolean(),
})
type CheckSettings = z.infer<typeof settingsSchema>
type SettingsResponse = {
  success: boolean
  message?: string
  data: CheckSettings
}
const endpoint = '/api/channel/astra_iq/settings'
const queryKey = ['channels', 'astra-iq-settings']
const formId = 'astra-iq-settings-form'

export function AstraIQSettings() {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button
        variant='outline'
        size='sm'
        className='h-6 shrink-0 gap-1 rounded-full px-2 text-xs font-normal'
        onClick={() => setOpen(true)}
      >
        {t('Check settings')}
        <Settings2 data-icon='inline-end' />
      </Button>
      {open && <SettingsDialog onClose={() => setOpen(false)} />}
    </>
  )
}

function SettingsDialog(props: { onClose: () => void }) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const client = useQueryClient()
  const query = useQuery({
    queryKey,
    queryFn: async ({ signal }) => {
      const response = await api.get<SettingsResponse>(endpoint, { signal })
      return settingsSchema.parse(requireServerSuccess(response.data).data)
    },
    gcTime: 0,
    refetchOnWindowFocus: false,
    retry: false,
    meta: { errorToast: false },
  })
  const mutation = useMutation({
    mutationFn: async (settings: CheckSettings) => {
      const response = await api.put<SettingsResponse>(endpoint, settings)
      return requireServerSuccess(response.data).data
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['channels', 'astra-iq'] })
      props.onClose()
    },
    onError: markServerErrorHandled,
    meta: { errorToast: false },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !mutation.isPending) props.onClose()
      }}
      title={t('Check settings')}
      description={t(
        'Applies to all Astra channels. Daily schedule in Beijing time (UTC+8).'
      )}
      contentClassName='sm:max-w-md'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            variant='outline'
            disabled={mutation.isPending}
            onClick={props.onClose}
          >
            {t('Cancel')}
          </Button>
          {canWrite && (
            <Button
              type='submit'
              form={formId}
              disabled={
                !query.isSuccess || query.isFetching || mutation.isPending
              }
            >
              {t('Save')}
            </Button>
          )}
        </>
      }
    >
      {query.isPending && <LoadingState />}
      {query.isError && (
        <ErrorState
          className='min-h-40'
          title={t('Check settings unavailable')}
          onRetry={() => void query.refetch()}
        />
      )}
      {query.isSuccess && (
        <SettingsForm
          key={query.dataUpdatedAt}
          settings={query.data}
          disabled={!canWrite || query.isFetching || mutation.isPending}
          onSave={(values) => mutation.mutate(values)}
        />
      )}
      {mutation.isError && (
        <p role='alert' className='text-destructive text-sm'>
          {t(getServerErrorMessage(mutation.error))}
        </p>
      )}
    </Dialog>
  )
}

function SettingsForm(props: {
  settings: CheckSettings
  disabled: boolean
  onSave: (settings: CheckSettings) => void
}) {
  const { t } = useTranslation()
  const form = useForm<CheckSettings>({
    resolver: zodResolver(settingsSchema),
    defaultValues: props.settings,
  })
  return (
    <Form {...form}>
      <form
        id={formId}
        noValidate
        className='space-y-5'
        onSubmit={form.handleSubmit(props.onSave)}
      >
        <FormField
          control={form.control}
          name='enabled'
          render={({ field }) => (
            <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
              <div className='space-y-1'>
                <FormLabel>{t('Enable checks')}</FormLabel>
                <FormDescription>
                  {t(
                    'When off, checks stop and results do not block calls. History is kept.'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  name={field.name}
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  onBlur={field.onBlur}
                  ref={field.ref}
                  disabled={props.disabled}
                />
              </FormControl>
            </FormItem>
          )}
        />
        <div className='grid grid-cols-2 gap-4'>
          {(['start_time', 'end_time'] as const).map((name) => (
            <FormField
              key={name}
              control={form.control}
              name={name}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {name === 'start_time'
                      ? t('Check start time')
                      : t('End Time')}
                  </FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      type='time'
                      step={60}
                      disabled={props.disabled}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          ))}
        </div>
        <p className='text-muted-foreground text-xs'>
          {t('Equal times mean all day. Overnight schedules are supported.')}
        </p>
        <FormField
          control={form.control}
          name='interval_minutes'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Interval (minutes)')}</FormLabel>
              <FormControl>
                <Input
                  {...safeNumberFieldProps(field)}
                  type='number'
                  min={1}
                  max={1440}
                  step={1}
                  disabled={props.disabled}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='stop_on_failure'
          render={({ field }) => (
            <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
              <div className='space-y-1'>
                <FormLabel>{t('Stop on failure')}</FormLabel>
                <FormDescription>
                  {t(
                    'Pause Astra calls after a failed check. Calls without a result are allowed.'
                  )}
                </FormDescription>
              </div>
              <FormControl>
                <Switch
                  name={field.name}
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  onBlur={field.onBlur}
                  ref={field.ref}
                  disabled={props.disabled}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      </form>
    </Form>
  )
}
