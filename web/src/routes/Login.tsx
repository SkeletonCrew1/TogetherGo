import { useState } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'

import { useAuth } from '@/auth/AuthContext'
import { Alert } from '@/components/ui/Alert'
import { Button } from '@/components/ui/Button'
import { Input } from '@/components/ui/Field'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { AuthShell } from './AuthShell'

/**
 * Client-side validation is deliberately thin here: an email that looks like an
 * email and a password that is not empty. Everything else about a password is
 * identity's business, and duplicating its strength rules on a *login* form
 * would lock out an account that predates them.
 */
const schema = z.object({
  email: z.string().min(1, 'Enter your email').email('That does not look like an email address'),
  password: z.string().min(1, 'Enter your password'),
})

type Values = z.infer<typeof schema>

export function Login() {
  const { login } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const [formError, setFormError] = useState<string | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<Values>({ resolver: zodResolver(schema) })

  // Where they were headed before the guard sent them here.
  const destination = (location.state as { from?: { pathname: string } } | null)?.from?.pathname

  const onSubmit = async (values: Values) => {
    setFormError(null)
    try {
      await login(values)
      navigate(destination ?? '/main', { replace: true })
    } catch (error) {
      // Identity answers a wrong password and an unknown email identically, and
      // so does this form. Telling a stranger which of the two it was is an
      // account-enumeration oracle.
      if (error instanceof ApiError && error.status === 401) {
        setFormError('That email and password do not match an account.')
        return
      }
      setFormError(errorMessage(error))
    }
  }

  return (
    <AuthShell
      title="Welcome back"
      subtitle="Sign in to find people to travel with."
      footer={
        <>
          New here?{' '}
          <Link to="/registration" className="font-medium text-brand-700 hover:underline">
            Create an account
          </Link>
        </>
      }
    >
      <form onSubmit={handleSubmit(onSubmit)} noValidate className="space-y-4">
        {formError && <Alert>{formError}</Alert>}

        <Input
          label="Email"
          type="email"
          autoComplete="email"
          autoFocus
          error={errors.email?.message}
          {...register('email')}
        />
        <Input
          label="Password"
          type="password"
          autoComplete="current-password"
          error={errors.password?.message}
          {...register('password')}
        />

        <Button type="submit" size="lg" className="w-full" isLoading={isSubmitting}>
          Sign in
        </Button>
      </form>
    </AuthShell>
  )
}
