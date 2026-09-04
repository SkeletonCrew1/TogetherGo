import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'

import { useAuth } from '@/auth/AuthContext'
import { Alert } from '@/components/ui/Alert'
import { Button } from '@/components/ui/Button'
import { Input, Textarea } from '@/components/ui/Field'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { AuthShell } from './AuthShell'

const MINIMUM_AGE = 16
const MAXIMUM_AGE = 120
const PASSWORD_MIN = 10
const BIO_MAX = 500
const FULL_NAME_MAX = 64

/** Completed years, the same arithmetic identity's `age_on` does. */
function ageOn(birthDate: Date, today: Date): number {
  let age = today.getFullYear() - birthDate.getFullYear()
  const beforeBirthday =
    today.getMonth() < birthDate.getMonth() ||
    (today.getMonth() === birthDate.getMonth() && today.getDate() < birthDate.getDate())
  if (beforeBirthday) age -= 1
  return age
}

/**
 * A mirror of identity's own rules (`app/schemas/auth.py`,
 * `app/schemas/password_policy.py`), so that the common mistakes are caught
 * before a round trip.
 *
 * A mirror, not the authority — identity revalidates everything and its answer
 * wins. The rules are duplicated because the alternative is a form that lets
 * someone type a nine-character password, submit, and get it back as a field
 * error; and duplicating them is exactly the trade CLAUDE.md already makes for
 * the event envelope. When they drift, the server's 422 still lands on the
 * right field via `fieldErrors()` below.
 */
const schema = z
  .object({
    full_name: z
      .string()
      .trim()
      .min(1, 'Enter your name')
      .max(FULL_NAME_MAX, `At most ${FULL_NAME_MAX} characters`),
    bio: z.string().trim().max(BIO_MAX, `At most ${BIO_MAX} characters`).optional(),
    birth_date: z
      .string()
      .min(1, 'Enter your date of birth')
      .refine((value) => !Number.isNaN(Date.parse(value)), 'That is not a valid date')
      .refine((value) => new Date(value) <= new Date(), 'That date is in the future')
      .refine(
        (value) => ageOn(new Date(value), new Date()) >= MINIMUM_AGE,
        `You must be at least ${MINIMUM_AGE} to sign up`,
      )
      .refine(
        (value) => ageOn(new Date(value), new Date()) <= MAXIMUM_AGE,
        'That is not a plausible date of birth',
      ),
    email: z.string().min(1, 'Enter your email').email('That does not look like an email address'),
    password: z
      .string()
      .min(PASSWORD_MIN, `At least ${PASSWORD_MIN} characters`)
      .max(256, 'At most 256 characters')
      // `isdigit` on the server is true for unicode digits too; a plain regex
      // would miss "²²²²²²²²²²", so this checks for the absence of a non-digit.
      .refine((value) => !/^\p{Nd}+$/u.test(value), 'Cannot be only digits'),
  })
  .refine(
    (values) => {
      const localPart = values.email.split('@')[0] ?? ''
      if (localPart.length < 3) return true
      return !values.password.toLowerCase().includes(localPart.toLowerCase())
    },
    { path: ['password'], message: 'Cannot contain the first part of your email address' },
  )

type Values = z.infer<typeof schema>

/** The names a server-side field error may be mapped back onto. */
const FIELD_NAMES = ['full_name', 'bio', 'birth_date', 'email', 'password'] as const

export function Registration() {
  const { register: signUp } = useAuth()
  const navigate = useNavigate()
  const [formError, setFormError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<Values>({ resolver: zodResolver(schema) })

  const onSubmit = async (values: Values) => {
    setFormError(null)
    try {
      await signUp({
        email: values.email,
        password: values.password,
        full_name: values.full_name,
        birth_date: values.birth_date,
        bio: values.bio ? values.bio : null,
      })
      // Registration signs you in — identity returns a token pair alongside the
      // account — so there is nothing to do but go to the app.
      navigate('/main', { replace: true })
    } catch (error) {
      if (error instanceof ApiError) {
        if (error.code === 'email_taken' || error.status === 409) {
          setError('email', { message: 'An account with that email already exists' })
          return
        }
        // A 422 from FastAPI carries per-field messages; putting them back on
        // the fields is what keeps the two validators from disagreeing
        // invisibly.
        const fields = error.fieldErrors()
        if (fields.length > 0) {
          for (const { field, message } of fields) {
            // FastAPI reports a body field as "body.email"; the last segment is
            // the name this form knows it by.
            const name = field.split('.').pop()
            if (name && (FIELD_NAMES as readonly string[]).includes(name)) {
              setError(name as keyof Values, { message })
            }
          }
          setFormError('Check the highlighted fields.')
          return
        }
      }
      setFormError(errorMessage(error))
    }
  }

  return (
    <AuthShell
      title="Create your account"
      subtitle="Tell people who they would be travelling with."
      wide
      footer={
        <>
          Already have an account?{' '}
          <Link to="/login" className="font-medium text-brand-700 hover:underline">
            Sign in
          </Link>
        </>
      }
    >
      <form onSubmit={handleSubmit(onSubmit)} noValidate className="space-y-4">
        {formError && <Alert>{formError}</Alert>}

        <Input
          label="Full name"
          autoComplete="name"
          autoFocus
          maxLength={FULL_NAME_MAX}
          error={errors.full_name?.message}
          {...register('full_name')}
        />

        <Textarea
          label="About me"
          rows={3}
          maxLength={BIO_MAX}
          placeholder="What you like about travelling, the pace you prefer, languages you speak…"
          hint="Optional, and the first thing an organizer reads about you."
          error={errors.bio?.message}
          {...register('bio')}
        />

        <Input
          label="Date of birth"
          type="date"
          autoComplete="bday"
          // Not enforcement — the schema does that, and a `max` attribute is
          // trivially removed. It is a nicer date picker.
          max={new Date().toISOString().slice(0, 10)}
          hint="Only your age is ever shown to other travellers."
          error={errors.birth_date?.message}
          {...register('birth_date')}
        />

        <Input
          label="Email"
          type="email"
          autoComplete="email"
          error={errors.email?.message}
          {...register('email')}
        />

        <Input
          label="Password"
          type="password"
          autoComplete="new-password"
          hint={`At least ${PASSWORD_MIN} characters.`}
          error={errors.password?.message}
          {...register('password')}
        />

        <Button type="submit" size="lg" className="w-full" isLoading={isSubmitting}>
          Create account
        </Button>
      </form>
    </AuthShell>
  )
}
