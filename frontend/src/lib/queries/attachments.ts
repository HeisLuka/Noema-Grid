import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'

// Page attachments — the files (space_files) parented to a page, including any
// rclone-synced into its folder. Backed by /api/pages/{id}/attachments (list +
// upload + delete) with bytes served from the public, content-addressed
// /api/files/{space_id}/{hash}. See backend attachments.go.

export interface Attachment {
  id: number
  name: string
  mime: string
  byte_size: number
  hash: string
  /** Stable, rename-proof serve URL (content-addressed) — the blob, for the body. */
  url: string
  /**
   * The file page (`/f/{hash}/{name}`) — the link to give a PERSON: it previews
   * the file and unfurls as a card, where `url` downloads and unfurls as
   * nothing. Server-computed; never re-derive it client-side.
   */
  share_path: string
  /** The page body already references this file's hash (it's embedded inline). */
  embedded: boolean
  /**
   * Machine-generated 1-2 sentence summary of the file's extracted text. Empty
   * until the background worker runs, or for non-text files (images, binaries).
   */
  summary?: string
}

export const attachmentKeys = {
  all: ['attachments'] as const,
  page: (pageId: number) => [...attachmentKeys.all, 'page', pageId] as const,
}

export function useAttachments(pageId: number | null | undefined) {
  return useQuery({
    queryKey: attachmentKeys.page(pageId ?? -1),
    queryFn: async () => {
      const { attachments } = await api<{ attachments: Attachment[] }>(
        `/api/pages/${pageId}/attachments`,
      )
      return attachments
    },
    enabled: pageId != null && pageId > 0,
    staleTime: 10_000,
  })
}

// uploadAttachment posts a single file (multipart) and returns its stored
// metadata. Used by the editor drop/paste path and the strip's add button. Uses
// raw fetch (not api()) because the body is FormData — api() forces JSON.
export async function uploadAttachment(
  pageId: number,
  file: File,
): Promise<Attachment> {
  const form = new FormData()
  form.append('file', file)
  const res = await fetch(`/api/pages/${pageId}/attachments`, {
    method: 'POST',
    body: form,
    credentials: 'include',
  })
  if (!res.ok) throw new Error(`upload failed: ${res.status}`)
  const { attachment } = (await res.json()) as { attachment: Attachment }
  return attachment
}

export function useDeleteAttachment(pageId: number) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (fileId: number) => {
      await api<void>(`/api/pages/${pageId}/attachments/${fileId}`, {
        method: 'DELETE',
      })
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: attachmentKeys.page(pageId) })
    },
  })
}
