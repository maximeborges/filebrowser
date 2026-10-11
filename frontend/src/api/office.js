import { notify } from "@/notify/index.ts"
import { getApiPath, getPublicApiPath } from '@/utils/url.js'
import { fetchURL } from "./utils.ts"

// GET /api/office/config or /public/api/office/config
export async function getConfig(req) {
  try {
    const params = {
      path: req.path,
      ...(req.hash && { hash: req.hash }),
      ...(req.source && { source: req.source }),
    }

    const headers = {}
    if (req.hash) {
      const sharePassword = localStorage.getItem(`sharepass:${req.hash}`)
      if (sharePassword) {
        headers['X-SHARE-PASSWORD'] = encodeURIComponent(sharePassword)
      }
    }

    let apiPath
    if (req.hash) {
      apiPath = getPublicApiPath('office/config', params)
    } else {
      apiPath = getApiPath('office/config', params)
    }

    const res = await fetchURL(apiPath, { headers })
    return await res.json()
  } catch (err) {
    notify.showError(err.message || 'Error fetching OnlyOffice configuration')
    throw err
  }
}

// POST /api/office/callback or /public/api/office/callback
export async function callback(params, hash) {
  try {
    let apiPath
    if (hash) {
      apiPath = getPublicApiPath('office/callback', { hash, ...params })
    } else {
      apiPath = getApiPath('office/callback', params)
    }

    const res = await fetchURL(apiPath, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(params)
    })
    return await res.json()
  } catch (err) {
    notify.showError(err.message || 'Error sending OnlyOffice callback')
    throw err
  }
}

// GET /api/office/history: versions for the editor's refreshHistory
export async function getHistory(req) {
  const res = await fetchURL(getApiPath('office/history', { path: req.path, source: req.source }))
  return await res.json()
}

// GET /api/office/history/data: one version for the editor's setHistoryData
export async function getHistoryData(req, version) {
  const res = await fetchURL(getApiPath('office/history/data', { path: req.path, source: req.source, version }))
  return await res.json()
}
