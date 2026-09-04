import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useMutation, useQueryClient } from '@tanstack/react-query'

import { useAuth, useCurrentUser } from '@/auth/AuthContext'
import { Alert } from '@/components/ui/Alert'
import { Avatar } from '@/components/ui/Avatar'
import { Button } from '@/components/ui/Button'
import { Input, Textarea } from '@/components/ui/Field'
import { Modal } from '@/components/ui/Modal'
import { RatingStars } from '@/components/ui/Stars'
import { keys } from '@/hooks/queryKeys'
import * as usersApi from '@/lib/api/users'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { cx, formatDate } from '@/lib/format'
import type { UpdateProfileBody } from '@/lib/types'

/**
 * One editable field.
 *
 * Each row is its own form and its own PATCH. That follows the endpoint: PATCH
 * /api/users/me works from the keys that were actually sent, so a single-field
 * save is a single-field change and never overwrites something the user has not
 * looked at.
 *
 * `null` is a real instruction on the three nullable fields — an empty box means
 * clear it, which is not the same as leaving it alone.
 */
function EditableField({
  label,
  value,
  placeholder,
  multiline = false,
  type = 'text',
  maxLength,
  hint,
  nullable = true,
  onSave,
  validate,
}: {
  label: string
  value: string | null
  placeholder?: string
  multiline?: boolean
  type?: string
  maxLength?: number
  hint?: string
  nullable?: boolean
  onSave: (next: string | null) => Promise<void>
  validate?: (next: string) => string | null
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value ?? '')
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  const start = () => {
    setDraft(value ?? '')
    setError(null)
    setEditing(true)
  }

  const save = async () => {
    const trimmed = draft.trim()

    if (!trimmed && !nullable) {
      setError(`${label} cannot be empty.`)
      return
    }
    const invalid = trimmed && validate ? validate(trimmed) : null
    if (invalid) {
      setError(invalid)
      return
    }

    setSaving(true)
    setError(null)
    try {
      await onSave(trimmed ? trimmed : null)
      setEditing(false)
    } catch (cause) {
      if (cause instanceof ApiError) {
        const field = cause.fieldErrors()[0]
        setError(field ? field.message : cause.message)
      } else {
        setError(errorMessage(cause))
      }
    } finally {
      setSaving(false)
    }
  }

  if (!editing) {
    return (
      <div className="flex items-start justify-between gap-4 border-b border-slate-100 py-3 last:border-0">
        <div className="min-w-0">
          <dt className="text-xs font-medium uppercase tracking-wide text-slate-500">{label}</dt>
          <dd
            className={cx(
              'mt-0.5 whitespace-pre-wrap',
              value ? 'text-slate-900' : 'italic text-slate-400',
            )}
          >
            {value || 'Not set'}
          </dd>
        </div>
        <button
          type="button"
          onClick={start}
          className="shrink-0 rounded px-2 py-1 text-sm font-medium text-brand-700 hover:bg-brand-50"
        >
          Edit
        </button>
      </div>
    )
  }

  return (
    <form
      className="border-b border-slate-100 py-3 last:border-0"
      onSubmit={(event) => {
        event.preventDefault()
        void save()
      }}
    >
      {multiline ? (
        <Textarea
          label={label}
          rows={4}
          autoFocus
          maxLength={maxLength}
          placeholder={placeholder}
          hint={hint}
          error={error ?? undefined}
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
      ) : (
        <Input
          label={label}
          type={type}
          autoFocus
          maxLength={maxLength}
          placeholder={placeholder}
          hint={hint}
          error={error ?? undefined}
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
      )}

      <div className="mt-2 flex gap-2">
        <Button type="submit" size="sm" isLoading={saving}>
          Save
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={() => setEditing(false)}>
          Cancel
        </Button>
      </div>
    </form>
  )
}

export function Profile() {
  const me = useCurrentUser()
  const { setUser, logout } = useAuth()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [passwordOpen, setPasswordOpen] = useState(false)

  const updateMutation = useMutation({
    mutationFn: (body: UpdateProfileBody) => usersApi.updateMe(body),
    onSuccess: async (profile) => {
      setUser(profile)
      // The name and photo shown on trip cards and rosters came from identity
      // via a different query; drop them so the change is visible everywhere.
      await queryClient.invalidateQueries({ queryKey: keys.users.detail(profile.id) })
    },
  })

  const save = (body: UpdateProfileBody) => updateMutation.mutateAsync(body).then(() => undefined)

  const signOut = async () => {
    await logout()
    navigate('/login', { replace: true })
  }

  return (
    <div className="mx-auto max-w-3xl px-4 py-6">
      <h1 className="text-2xl font-semibold text-slate-900">Profile</h1>

      <section className="mt-5 rounded-xl border border-slate-200 bg-white p-5">
        <div className="flex flex-wrap items-center gap-5">
          <Avatar name={me.full_name} photoUrl={me.photo_url} size="xl" />
          <div className="min-w-0">
            <h2 className="text-xl font-semibold text-slate-900">{me.full_name}</h2>
            <p className="text-sm text-slate-500">
              {me.age !== null ? `${me.age} · ` : ''}
              Joined {formatDate(me.created_at)}
            </p>
            <div className="mt-2">
              <RatingStars value={me.rating_avg} count={me.rating_count} />
            </div>
            {me.rating_count === 0 && (
              <p className="mt-1 text-xs text-slate-400">
                Your average appears once co-travellers have rated you.
              </p>
            )}
          </div>
        </div>
      </section>

      <section className="mt-5 rounded-xl border border-slate-200 bg-white px-5 py-2">
        <dl>
          <EditableField
            label="Full name"
            value={me.full_name}
            maxLength={64}
            nullable={false}
            onSave={(next) => save({ full_name: next as string })}
          />

          <EditableField
            label="About me"
            value={me.bio}
            multiline
            maxLength={500}
            placeholder="What you like about travelling, the pace you prefer, languages you speak…"
            onSave={(next) => save({ bio: next })}
          />

          <EditableField
            label="Phone"
            value={me.phone}
            type="tel"
            placeholder="+48123456789"
            hint="International format, starting with +. Never shown to other travellers."
            // Identity's own pattern. Checking it here means the error appears
            // under the field instead of arriving as a 422.
            validate={(next) =>
              /^\+[1-9]\d{6,14}$/.test(next)
                ? null
                : 'Use international format: + then 7–15 digits.'
            }
            onSave={(next) => save({ phone: next })}
          />

          <EditableField
            label="Photo URL"
            value={me.photo_url}
            type="url"
            maxLength={2048}
            placeholder="https://…"
            hint="A link to an image. There are no uploads in this build."
            validate={(next) => {
              try {
                const url = new URL(next)
                return url.protocol === 'http:' || url.protocol === 'https:'
                  ? null
                  : 'Must be an http or https URL.'
              } catch {
                return 'That is not a valid URL.'
              }
            }}
            onSave={(next) => save({ photo_url: next })}
          />

          <div className="flex items-start justify-between gap-4 border-b border-slate-100 py-3">
            <div>
              <dt className="text-xs font-medium uppercase tracking-wide text-slate-500">Email</dt>
              <dd className="mt-0.5 text-slate-900">{me.email}</dd>
            </div>
            {/* No endpoint changes an email address, so this is not offered as
                an editable field. */}
            <span className="shrink-0 pt-1 text-xs text-slate-400">Cannot be changed</span>
          </div>

          <div className="flex items-start justify-between gap-4 py-3">
            <div>
              <dt className="text-xs font-medium uppercase tracking-wide text-slate-500">
                Date of birth
              </dt>
              <dd className="mt-0.5 text-slate-900">
                {me.birth_date ? formatDate(me.birth_date) : 'Not set'}
              </dd>
            </div>
            <span className="shrink-0 pt-1 text-xs text-slate-400">Only your age is shared</span>
          </div>
        </dl>
      </section>

      {updateMutation.error && (
        <Alert className="mt-4">{errorMessage(updateMutation.error)}</Alert>
      )}

      <section className="mt-5 flex flex-wrap gap-2 rounded-xl border border-slate-200 bg-white p-5">
        <Button variant="secondary" onClick={() => setPasswordOpen(true)}>
          Change password
        </Button>
        <Button variant="ghost" className="text-rose-700 hover:bg-rose-50" onClick={signOut}>
          Log out
        </Button>
      </section>

      <ChangePasswordDialog open={passwordOpen} onClose={() => setPasswordOpen(false)} />
    </div>
  )
}

/**
 * Changing the password ends every other session — identity revokes the whole
 * refresh chain. This one keeps working because the response is a fresh pair,
 * but the dialog says so, because a user who is signed in on a phone should
 * find out here rather than there.
 */
function ChangePasswordDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [done, setDone] = useState(false)

  const mutation = useMutation({
    mutationFn: () => usersApi.changePassword({ current_password: current, new_password: next }),
    onSuccess: () => {
      setDone(true)
      setCurrent('')
      setNext('')
    },
    onError: (cause) => {
      if (cause instanceof ApiError) {
        const field = cause.fieldErrors()[0]
        setError(field ? field.message : cause.message)
        return
      }
      setError(errorMessage(cause))
    },
  })

  const close = () => {
    setError(null)
    setDone(false)
    onClose()
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Change password"
      footer={
        done ? (
          <Button onClick={close}>Done</Button>
        ) : (
          <>
            <Button variant="secondary" onClick={close}>
              Cancel
            </Button>
            <Button
              isLoading={mutation.isPending}
              onClick={() => {
                setError(null)
                if (next.length < 10) {
                  setError('The new password needs at least 10 characters.')
                  return
                }
                if (next === current) {
                  setError('The new password has to differ from the current one.')
                  return
                }
                mutation.mutate()
              }}
            >
              Change it
            </Button>
          </>
        )
      }
    >
      {done ? (
        <Alert tone="success">
          Password changed. Any other device you were signed in on has been signed out.
        </Alert>
      ) : (
        <div className="space-y-4">
          {error && <Alert>{error}</Alert>}
          <Input
            label="Current password"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(event) => setCurrent(event.target.value)}
          />
          <Input
            label="New password"
            type="password"
            autoComplete="new-password"
            hint="At least 10 characters, and not only digits."
            value={next}
            onChange={(event) => setNext(event.target.value)}
          />
          <p className="text-sm text-slate-500">
            This signs you out everywhere else.
          </p>
        </div>
      )}
    </Modal>
  )
}
