/**
 * Redeem code API endpoints
 * Handles redeem code redemption for users
 */

import { apiClient } from './client'
import type { RedeemCodeRequest, PaginatedResponse } from '@/types'

export interface RedeemHistoryItem {
  id: number
  code: string
  type: string
  value: number
  status: string
  used_at: string
  created_at: string
  // Notes from admin for admin_balance/admin_concurrency types
  notes?: string
  // Subscription-specific fields
  group_id?: number
  validity_days?: number
  group?: {
    id: number
    name: string
  }
}

/**
 * Redeem a code
 * @param code - Redeem code string
 * @returns Redemption result with updated balance or concurrency
 */
export async function redeem(code: string): Promise<{
  message: string
  type: string
  value: number
  new_balance?: number
  new_concurrency?: number
}> {
  const payload: RedeemCodeRequest = { code }

  const { data } = await apiClient.post<{
    message: string
    type: string
    value: number
    new_balance?: number
    new_concurrency?: number
  }>('/redeem', payload)

  return data
}

export interface RedeemHistoryParams {
  page?: number
  page_size?: number
  /** Optional type filter, e.g. 'balance' / 'subscription' */
  type?: string
}

/**
 * Get user's redemption history (paginated)
 * @param params - Pagination and optional type filter
 * @returns Paginated list of redeemed codes
 */
export async function getHistory(
  params: RedeemHistoryParams = {}
): Promise<PaginatedResponse<RedeemHistoryItem>> {
  const { data } = await apiClient.get<PaginatedResponse<RedeemHistoryItem>>('/redeem/history', {
    params
  })
  return data
}

export const redeemAPI = {
  redeem,
  getHistory
}

export default redeemAPI
