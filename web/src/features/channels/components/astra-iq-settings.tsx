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
import { useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect } from '@/components/multi-select'
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

import { CHANNEL_STATUS, CHANNEL_STATUS_LABELS } from '../constants'

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
  channel_ids: z.array(z.number().int().positive()),
})
// Only channel metadata is returned; upstream credentials never reach the form.
const channelsSchema = z.array(
  z.object({
    id: z.number(),
    name: z.string(),
    status: z.number(),
    models: z.array(z.string()).default(['gpt-6-astra']),
  })
)
const configSchema = z.object({
  models: z.array(settingsSchema.extend({ model: z.string().min(1) })),
})
const resolveSettings = zodResolver(configSchema)
type CheckSettings = z.infer<typeof settingsSchema>
type CheckConfig = { model_settings: Record<string, CheckSettings> }
type FormValues = z.infer<typeof configSchema>
type CheckChannel = z.infer<typeof channelsSchema>[number]
type SettingsResponse = {
  success: boolean
  message?: string
  data: CheckSettings & Partial<CheckConfig> & { channels: CheckChannel[] }
}
const endpoint = '/api/channel/astra_iq/settings'
const queryKey = ['channels', 'astra-iq-settings']
const defaultModelSettings: CheckSettings = {
  enabled: true,
  start_time: '00:00',
  end_time: '00:00',
  interval_minutes: 5,
  stop_on_failure: true,
  channel_ids: [],
}
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
      const { channels, model_settings, ...settings } = requireServerSuccess(
        response.data
      ).data
      return {
        settings: model_settings
          ? z.record(z.string(), settingsSchema).parse(model_settings)
          : { 'gpt-6-astra': settingsSchema.parse(settings) },
        channels: channelsSchema.parse(channels),
      }
    },
    gcTime: 0,
    refetchOnWindowFocus: false,
    retry: false,
    meta: { errorToast: false },
  })
  const mutation = useMutation({
    mutationFn: async (settings: CheckConfig) => {
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
        'Each model has its own channels and schedule in Beijing time (UTC+8).'
      )}
      contentClassName='sm:max-w-lg'
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
          settings={query.data.settings}
          channels={query.data.channels}
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
  settings: Record<string, CheckSettings>
  channels: CheckChannel[]
  disabled: boolean
  onSave: (settings: CheckConfig) => void
}) {
  const { t } = useTranslation()
  const [activeModel, setActiveModel] = useState(
    Object.keys(props.settings)[0] ?? ''
  )
  const drafts = useRef<Record<string, CheckSettings>>({ ...props.settings })
  const form = useForm<FormValues>({
    resolver: (values, context, options) =>
      resolveSettings(
        {
          models: values.models.map((entry) =>
            entry.enabled
              ? entry
              : {
                  ...(props.settings[entry.model] ?? defaultModelSettings),
                  model: entry.model,
                  enabled: false,
                }
          ),
        },
        context,
        options
      ),
    defaultValues: {
      models: Object.entries(props.settings).map(([model, settings]) => ({
        model,
        ...settings,
      })),
    },
  })
  const models = form.watch('models')
  const index = models.findIndex((entry) => entry.model === activeModel)
  const enabled = index >= 0 && models[index].enabled
  const name = <K extends keyof CheckSettings>(key: K) =>
    `models.${index}.${key}` as const
  const selectedModels = models.map((entry) => entry.model)
  const availableModels = new Set(
    props.channels.flatMap((channel) => channel.models)
  )
  for (const model of selectedModels) availableModels.add(model)
  const channelOptions = props.channels.filter((channel) =>
    channel.models.includes(activeModel)
  )
  const options = channelOptions.map((channel) => {
    let label = `#${channel.id} ${channel.name}`
    if (channel.status !== CHANNEL_STATUS.ENABLED) {
      const status =
        CHANNEL_STATUS_LABELS[
          channel.status as keyof typeof CHANNEL_STATUS_LABELS
        ] ?? 'Unknown'
      label += ` · ${t(status)}`
    }
    return { value: String(channel.id), label }
  })
  for (const id of models[index]?.channel_ids ?? []) {
    if (!channelOptions.some((channel) => channel.id === id)) {
      options.push({ value: String(id), label: `#${id}` })
    }
  }
  return (
    <Form {...form}>
      <form
        id={formId}
        noValidate
        className='space-y-5'
        onSubmit={form.handleSubmit(
          (values) =>
            props.onSave({
              model_settings: Object.fromEntries(
                values.models.map(({ model, ...settings }) => [model, settings])
              ),
            }),
          (errors) => {
            const invalidIndex = errors.models?.findIndex?.((error) =>
              Boolean(error)
            )
            if (invalidIndex !== undefined && invalidIndex >= 0) {
              setActiveModel(models[invalidIndex].model)
            }
          }
        )}
      >
        <div className='space-y-2'>
          <label className='text-sm font-medium' htmlFor='iq-check-models'>
            {t('Check models')}
          </label>
          <MultiSelect
            id='iq-check-models'
            options={[...availableModels]
              .sort()
              .map((model) => ({ value: model, label: model }))}
            selected={selectedModels}
            activeValue={activeModel}
            onChipClick={setActiveModel}
            onChange={(names) => {
              for (const { model, ...settings } of form.getValues('models')) {
                drafts.current[model] = settings
              }
              form.setValue(
                'models',
                names.map((model) => ({
                  model,
                  ...(drafts.current[model] ?? {
                    ...defaultModelSettings,
                    channel_ids: [],
                  }),
                }))
              )
              form.clearErrors()
              const addedModel = names.find(
                (model) => !selectedModels.includes(model)
              )
              if (addedModel) {
                setActiveModel(addedModel)
              } else if (!names.includes(activeModel)) {
                setActiveModel(names[0] ?? '')
              }
            }}
            placeholder={t('Select check models')}
            disabled={props.disabled}
          />
          <p className='text-muted-foreground text-xs'>
            {t(
              'Select models to check, then click a selected model to configure it. Removing a model keeps its history and stops gating its calls.'
            )}
          </p>
        </div>
        {models.length > 0 && (
          <>
            <FormField
              control={form.control}
              name={name('enabled')}
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
            {enabled && (
              <>
                <FormField
                  control={form.control}
                  name={name('channel_ids')}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Check channels')}</FormLabel>
                      <FormControl>
                        <MultiSelect
                          options={options}
                          selected={field.value.map(String)}
                          onChange={(values) =>
                            field.onChange(values.map(Number))
                          }
                          placeholder={t('All channels for this model')}
                          disabled={props.disabled}
                        />
                      </FormControl>
                      <FormDescription>
                        {t(
                          'Leave empty to check all channels for this model. Unselected channels are not checked and their results do not block calls.'
                        )}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                {/* iOS time controls can overrun width: 100% when padded. */}
                <div className='grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2'>
                  {(['start_time', 'end_time'] as const).map((timeField) => (
                    <FormField
                      key={timeField}
                      control={form.control}
                      name={name(timeField)}
                      render={({ field }) => (
                        <FormItem className='min-w-0 grid-cols-1'>
                          <FormLabel>
                            {timeField === 'start_time'
                              ? t('Check start time')
                              : t('End Time')}
                          </FormLabel>
                          <FormControl>
                            <Input
                              {...field}
                              type='time'
                              className='w-auto max-w-full appearance-none justify-self-stretch'
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
                  {t(
                    'Equal times mean all day. Overnight schedules are supported.'
                  )}
                </p>
                <FormField
                  control={form.control}
                  name={name('interval_minutes')}
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
                  name={name('stop_on_failure')}
                  render={({ field }) => (
                    <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
                      <div className='space-y-1'>
                        <FormLabel>{t('Stop on failure')}</FormLabel>
                        <FormDescription>
                          {t(
                            'Pause only this model on a failed channel. Calls without a result are allowed.'
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
              </>
            )}
          </>
        )}
      </form>
    </Form>
  )
}
