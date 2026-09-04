import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'

import { RouteEditor, type DraftPoint } from '@/components/map/RouteEditor'
import { Alert } from '@/components/ui/Alert'
import { Button } from '@/components/ui/Button'
import { Input, Select, Textarea } from '@/components/ui/Field'
import { keys } from '@/hooks/queryKeys'
import * as tripsApi from '@/lib/api/trips'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { CATEGORY_LABELS, fromDateTimeInput } from '@/lib/format'
import { CATEGORIES, TRIP_LIMITS, type CreateTripBody, type PointBody } from '@/lib/types'

/**
 * A mirror of `internal/domain/input.go`. The service revalidates all of it and
 * its answer wins; this exists so that the common mistakes are caught before a
 * round trip, and so the errors land on the fields that caused them.
 *
 * The route is validated outside this schema — it lives in its own state
 * because the map editor owns it, and a zod resolver cannot reach it.
 */
const schema = z
  .object({
    title: z
      .string()
      .trim()
      .min(TRIP_LIMITS.titleMin, `At least ${TRIP_LIMITS.titleMin} characters`)
      .max(TRIP_LIMITS.titleMax, `At most ${TRIP_LIMITS.titleMax} characters`),
    description: z
      .string()
      .trim()
      .max(TRIP_LIMITS.descriptionMax, `At most ${TRIP_LIMITS.descriptionMax} characters`)
      .optional(),
    category: z.enum(CATEGORIES),
    capacity: z.coerce
      .number()
      .int('Whole people only')
      .min(TRIP_LIMITS.capacityMin, `At least ${TRIP_LIMITS.capacityMin}`)
      .max(TRIP_LIMITS.capacityMax, `At most ${TRIP_LIMITS.capacityMax}`),
    start_at: z.string().min(1, 'When does it start?'),
    end_at: z.string().min(1, 'When does it end?'),
  })
  .refine((values) => new Date(values.end_at) > new Date(values.start_at), {
    path: ['end_at'],
    message: 'The end has to come after the start',
  })
  .refine(
    (values) => {
      const ms = new Date(values.end_at).getTime() - new Date(values.start_at).getTime()
      return ms <= TRIP_LIMITS.maxDurationDays * 86_400_000
    },
    {
      path: ['end_at'],
      message: `A trip cannot last longer than ${TRIP_LIMITS.maxDurationDays} days`,
    },
  )
  .refine((values) => new Date(values.start_at) > new Date(), {
    path: ['start_at'],
    message: 'The start has to be in the future',
  })

type Values = z.infer<typeof schema>

const FIELD_NAMES = ['title', 'description', 'category', 'capacity', 'start_at', 'end_at'] as const

export function OrganizeTrip() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [points, setPoints] = useState<DraftPoint[]>([])
  const [routeError, setRouteError] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    setError,
    formState: { errors },
  } = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { category: 'nature', capacity: 4 },
  })

  /**
   * Create, then optionally publish.
   *
   * Two calls rather than one, because the service has no "create published"
   * path: POST /api/trips always produces a draft (`StatusInitial = draft`) and
   * `/publish` is the only edge out of it. Saving as a draft is therefore the
   * cheaper action, and publishing is the draft plus one request.
   */
  const saveMutation = useMutation({
    mutationFn: async ({ body, publish }: { body: CreateTripBody; publish: boolean }) => {
      const trip = await tripsApi.create(body)
      if (!publish) return trip
      try {
        return await tripsApi.publish(trip.id)
      } catch (error) {
        // The trip exists; only the transition failed. Surfacing it as a total
        // failure would send the organizer to fill the form in again on top of
        // a draft that is already saved.
        throw new PublishFailed(trip.id, error)
      }
    },
    onSuccess: async (trip) => {
      await queryClient.invalidateQueries({ queryKey: keys.trips.all })
      await queryClient.invalidateQueries({ queryKey: keys.myTrips.all })
      navigate(`/trip/${trip.id}`)
    },
    onError: async (error) => {
      if (error instanceof PublishFailed) {
        await queryClient.invalidateQueries({ queryKey: keys.myTrips.all })
        setFormError(
          `The trip was saved as a draft, but publishing it failed: ${errorMessage(error.cause)}. You can publish it from the trip page.`,
        )
        navigate(`/trip/${error.tripId}`)
        return
      }

      if (error instanceof ApiError) {
        const fields = error.fieldErrors()
        const routeFields = fields.filter((field) => field.field.startsWith('points'))
        if (routeFields.length > 0) {
          setRouteError(routeFields.map((field) => `${field.field}: ${field.message}`).join('; '))
        }
        for (const { field, message } of fields) {
          if ((FIELD_NAMES as readonly string[]).includes(field)) {
            setError(field as keyof Values, { message })
          }
        }
        if (fields.length > 0) {
          setFormError('Check the highlighted fields.')
          return
        }
      }
      setFormError(errorMessage(error))
    },
  })

  const submit = (publish: boolean) =>
    handleSubmit((values) => {
      setFormError(null)
      setRouteError(null)

      // The route's rules, checked here because the map editor owns the state
      // and the resolver cannot see it.
      if (points.length < TRIP_LIMITS.pointsMin) {
        setRouteError(`Add at least ${TRIP_LIMITS.pointsMin} stops — where you leave from and where you are going.`)
        return
      }
      const unnamed = points.findIndex((point) => !point.name.trim())
      if (unnamed >= 0) {
        setRouteError(`Stop ${unnamed + 1} needs a name.`)
        return
      }

      const body: CreateTripBody = {
        title: values.title,
        description: values.description ? values.description : null,
        category: values.category,
        capacity: values.capacity,
        start_at: fromDateTimeInput(values.start_at),
        end_at: fromDateTimeInput(values.end_at),
        points: points.map(toPointBody),
      }

      saveMutation.mutate({ body, publish })
    })

  return (
    <div className="mx-auto max-w-4xl px-4 py-6">
      <h1 className="text-2xl font-semibold text-slate-900">Organize a trip</h1>
      <p className="mt-1 text-sm text-slate-600">
        Saved trips start as drafts — only you can see one until you publish it.
      </p>

      <form className="mt-6 space-y-6" noValidate>
        {formError && <Alert>{formError}</Alert>}

        <section className="space-y-4 rounded-xl border border-slate-200 bg-white p-5">
          <Input
            label="Title"
            autoFocus
            maxLength={TRIP_LIMITS.titleMax}
            placeholder="Three days in the Tatras"
            error={errors.title?.message}
            {...register('title')}
          />

          <Textarea
            label="Description"
            rows={5}
            maxLength={TRIP_LIMITS.descriptionMax}
            placeholder="The shape of the trip, what the days look like, who it suits…"
            hint="Optional, but the thing people read before deciding to ask."
            error={errors.description?.message}
            {...register('description')}
          />

          <div className="grid gap-4 sm:grid-cols-2">
            <Select label="Category" error={errors.category?.message} {...register('category')}>
              {CATEGORIES.map((category) => (
                <option key={category} value={category}>
                  {CATEGORY_LABELS[category]}
                </option>
              ))}
            </Select>

            <Input
              label="People"
              type="number"
              min={TRIP_LIMITS.capacityMin}
              max={TRIP_LIMITS.capacityMax}
              hint="Counting you."
              error={errors.capacity?.message}
              {...register('capacity')}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <Input
              label="Starts"
              type="datetime-local"
              error={errors.start_at?.message}
              {...register('start_at')}
            />
            <Input
              label="Ends"
              type="datetime-local"
              error={errors.end_at?.message}
              {...register('end_at')}
            />
          </div>
        </section>

        <section className="rounded-xl border border-slate-200 bg-white p-5">
          <h2 className="mb-1 text-sm font-semibold uppercase tracking-wide text-slate-500">
            Route
          </h2>
          <p className="mb-4 text-sm text-slate-500">
            The first stop is where you leave from and the last is the destination — that is what
            the departure-radius filter searches on.
          </p>
          <RouteEditor points={points} onChange={setPoints} error={routeError ?? undefined} />
        </section>

        <div className="flex flex-wrap justify-end gap-2">
          <Button
            type="button"
            variant="secondary"
            isLoading={saveMutation.isPending && !saveMutation.variables?.publish}
            onClick={submit(false)}
          >
            Save as draft
          </Button>
          <Button
            type="button"
            size="lg"
            isLoading={saveMutation.isPending && saveMutation.variables?.publish === true}
            onClick={submit(true)}
          >
            Publish trip
          </Button>
        </div>
      </form>
    </div>
  )
}

function toPointBody(point: DraftPoint): PointBody {
  return {
    name: point.name.trim(),
    lat: point.lat,
    lng: point.lng,
    arrive_at: point.arriveAt ? fromDateTimeInput(point.arriveAt) : null,
    transport: point.transport ? point.transport : null,
  }
}

/**
 * The trip was created and the publish was not.
 *
 * A distinct error type so the handler can navigate to the draft instead of
 * treating the whole submit as lost — the two halves of this mutation are not
 * atomic and cannot be.
 */
class PublishFailed extends Error {
  constructor(
    readonly tripId: string,
    readonly cause: unknown,
  ) {
    super('The trip was saved but not published.')
    this.name = 'PublishFailed'
  }
}
