import { api } from './client'
import type { PrivateProfile, PublicProfile, UpdateProfileBody } from '@/lib/types'

export const me = () => api.get<PrivateProfile>('/api/users/me')

export const updateMe = (body: UpdateProfileBody) =>
  api.patch<PrivateProfile>('/api/users/me', body)

export const changePassword = (body: { current_password: string; new_password: string }) =>
  api.post<void>('/api/users/me/password', body)

export const publicProfile = (userId: string) =>
  api.get<PublicProfile>(`/api/users/${userId}`)
