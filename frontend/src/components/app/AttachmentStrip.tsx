import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Check, Download, FileText, Link2, Paperclip, Trash2, CornerLeftUp } from 'lucide-react'
import { cn } from '../../lib/utils'
import { isPdf, PdfPreviewDialog } from '../ui/pdf-viewer'
import { fileShareUrl } from '../../lib/file-link'
import { Tooltip, TooltipContent, TooltipTrigger } from '../ui/tooltip'
import {
  attachmentKeys,
  useAttachments,
  useDeleteAttachment,
  type Attachment,
} from '../../lib/queries/attachments'

// The page Attachments strip — a compact chip row shown directly below the page
// title (reader + editor). Lists every file parented to the page (uploads AND
// files rclone-synced into its folder), so synced files are visible without any
// body edit. Images show as thumbnails, other files as icon chips. Embedded
// files (already placed inline in the body) carry a small marker so the manifest
// stays complete without being confusing. In editable mode each chip can be
// inserted into the body or deleted.

const COLLAPSED_LIMIT = 8

function isImageMime(mime: string): boolean {
  return mime.startsWith('image/')
}

function prettySize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const kb = bytes / 1024
  if (kb < 1024) return `${kb < 10 ? kb.toFixed(1) : Math.round(kb)} KB`
  const mb = kb / 1024
  return `${mb < 10 ? mb.toFixed(1) : Math.round(mb)} MB`
}

export interface AttachmentStripViewProps {
  attachments: Attachment[]
  editable?: boolean
  /** Insert a file into the body at the cursor (editor only). */
  onInsert?: (a: Attachment) => void
  /** Remove a file from the page (editor only). */
  onDelete?: (a: Attachment) => void
  deletingId?: number | null
  /** Start with the chip row revealed instead of the collapsed count toggle. */
  defaultOpen?: boolean
}

// Presentational strip — no data fetching, so it's directly Storybook-able.
export function AttachmentStripView({
  attachments,
  editable = false,
  onInsert,
  onDelete,
  deletingId,
  defaultOpen = false,
}: AttachmentStripViewProps) {
  // Collapsed by default — the chip row is visually loud, so a page leads with
  // its content and the files sit behind a quiet count toggle. `showAll` is the
  // second tier: once open, very long lists still cap at COLLAPSED_LIMIT.
  const [open, setOpen] = useState(defaultOpen)
  const [showAll, setShowAll] = useState(false)
  if (attachments.length === 0) return null

  if (!open) {
    return (
      <div className="py-[var(--space-2)]">
        <button
          type="button"
          onClick={() => setOpen(true)}
          aria-expanded={false}
          className={cn(
            'inline-flex items-center gap-[var(--space-2)]',
            'text-[length:var(--text-sm)] text-[var(--text-muted)]',
            'rounded-[var(--radius-sm)] px-[var(--space-2)] py-[var(--space-1)]',
            'hover:text-[var(--text-primary)] hover:bg-[var(--surface-3)]',
          )}
        >
          <Paperclip aria-hidden width={14} height={14} className="shrink-0" />
          {attachments.length}{' '}
          {attachments.length === 1 ? 'attachment' : 'attachments'}
        </button>
      </div>
    )
  }

  const hidden = Math.max(0, attachments.length - COLLAPSED_LIMIT)
  const shown = showAll ? attachments : attachments.slice(0, COLLAPSED_LIMIT)

  return (
    <div
      className="flex flex-wrap items-center gap-[var(--space-2)] py-[var(--space-2)]"
      aria-label={`Attachments (${attachments.length})`}
    >
      <button
        type="button"
        onClick={() => {
          setOpen(false)
          setShowAll(false)
        }}
        aria-label="Hide attachments"
        aria-expanded
        className="text-[var(--text-muted)] hover:text-[var(--text-primary)] shrink-0"
      >
        <Paperclip aria-hidden width={14} height={14} />
      </button>
      {shown.map((a) => (
        <AttachmentChip
          key={a.id}
          attachment={a}
          editable={editable}
          onInsert={onInsert}
          onDelete={onDelete}
          deleting={deletingId === a.id}
        />
      ))}
      {hidden > 0 && !showAll ? (
        <button
          type="button"
          onClick={() => setShowAll(true)}
          className={cn(
            'text-[length:var(--text-sm)] text-[var(--text-muted)]',
            'rounded-[var(--radius-sm)] px-[var(--space-2)] py-[var(--space-1)]',
            'hover:text-[var(--text-primary)] hover:bg-[var(--surface-3)]',
          )}
        >
          +{hidden} more
        </button>
      ) : null}
    </div>
  )
}

interface AttachmentChipProps {
  attachment: Attachment
  editable: boolean
  onInsert?: (a: Attachment) => void
  onDelete?: (a: Attachment) => void
  deleting?: boolean
}

function AttachmentChip({
  attachment: a,
  editable,
  onInsert,
  onDelete,
  deleting,
}: AttachmentChipProps) {
  const image = isImageMime(a.mime)
  const pdf = isPdf(a.name, a.mime)
  const [preview, setPreview] = useState(false)
  const [copied, setCopied] = useState(false)
  const summary = a.summary?.trim()
  // Copy the FILE PAGE link, not the blob URL: /api/files/… downloads on click
  // and unfurls as nothing wherever it's pasted. See lib/file-link.ts.
  const copyLink = async () => {
    if (!navigator.clipboard?.writeText) return
    await navigator.clipboard.writeText(fileShareUrl(a.hash, a.name))
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  const inner = (
    <>
      {image ? (
        <img
          src={a.url}
          alt=""
          className="h-5 w-5 rounded-[var(--radius-xs)] object-cover shrink-0"
        />
      ) : (
        <FileText
          aria-hidden
          width={16}
          height={16}
          className="text-[var(--text-muted)] shrink-0"
        />
      )}
      <span className="truncate text-[var(--text-primary)]">{a.name}</span>
      <span className="text-[var(--text-muted)] shrink-0">
        {prettySize(a.byte_size)}
      </span>
    </>
  )
  const chip = (
    <span
      className={cn(
        'group inline-flex items-center gap-[var(--space-2)] max-w-[16rem]',
        'rounded-[var(--radius-sm)] border border-[var(--border-subtle)]',
        'bg-[var(--surface-2)] pl-[var(--space-1)] pr-[var(--space-2)] py-[var(--space-1)]',
        'text-[length:var(--text-sm)]',
        deleting && 'opacity-50',
      )}
    >
      {pdf ? (
        <button
          type="button"
          onClick={() => setPreview(true)}
          className="inline-flex items-center gap-[var(--space-2)] min-w-0 cursor-pointer border-0 bg-transparent p-0 text-left"
          title={`Preview ${a.name}`}
        >
          {inner}
        </button>
      ) : (
        <a
          href={a.url}
          target="_blank"
          rel="noreferrer"
          download={image ? undefined : a.name}
          className="inline-flex items-center gap-[var(--space-2)] min-w-0"
          title={a.name}
        >
          {inner}
        </a>
      )}
      {pdf ? (
        <PdfPreviewDialog
          url={a.url}
          name={a.name}
          open={preview}
          onOpenChange={setPreview}
        />
      ) : null}
      <button
        type="button"
        onClick={() => void copyLink()}
        aria-label={`Copy a link to ${a.name}`}
        title="Copy link"
        className="shrink-0 text-[var(--text-muted)] hover:text-[var(--text-primary)]"
      >
        {copied ? (
          <Check width={13} height={13} aria-hidden />
        ) : (
          <Link2 width={13} height={13} aria-hidden />
        )}
      </button>
      {a.embedded ? (
        <CornerLeftUp
          aria-label="Embedded in page"
          width={13}
          height={13}
          className="text-[var(--text-muted)] shrink-0"
        />
      ) : null}
      {editable ? (
        <span className="inline-flex items-center gap-[var(--space-1)] shrink-0">
          {onInsert && !a.embedded ? (
            <button
              type="button"
              onClick={() => onInsert(a)}
              aria-label={`Insert ${a.name} into the page`}
              className="text-[var(--text-muted)] hover:text-[var(--text-primary)]"
            >
              <Download width={13} height={13} className="rotate-180" aria-hidden />
            </button>
          ) : null}
          {onDelete ? (
            <button
              type="button"
              onClick={() => onDelete(a)}
              disabled={deleting}
              aria-label={`Remove ${a.name}`}
              className="text-[var(--text-muted)] hover:text-[var(--accent-negative-fg)]"
            >
              <Trash2 width={13} height={13} aria-hidden />
            </button>
          ) : null}
        </span>
      ) : null}
    </span>
  )

  // When the background worker has summarized the file's text, hovering the chip
  // reveals the standfirst — the same affordance the page title gets (SummaryHint).
  if (!summary) return chip
  return (
    <Tooltip>
      <TooltipTrigger asChild>{chip}</TooltipTrigger>
      <TooltipContent
        side="bottom"
        align="start"
        className="max-w-[24rem] p-[var(--space-3)] text-[length:var(--text-sm)] leading-[var(--leading-normal)]"
      >
        <p className="m-0 mb-[var(--space-1)] text-[length:var(--text-xs)] text-[var(--text-muted)] font-[family-name:var(--font-mono)]">
          file summary
        </p>
        <p className="m-0">{summary}</p>
      </TooltipContent>
    </Tooltip>
  )
}

export interface AttachmentStripProps {
  pageId: number
  editable?: boolean
  onInsert?: (a: Attachment) => void
}

// Data container: fetches the page's attachments and wires delete. Renders
// nothing while empty/loading so it never adds blank chrome to a page.
export function AttachmentStrip({
  pageId,
  editable = false,
  onInsert,
}: AttachmentStripProps) {
  const { data } = useAttachments(pageId)
  const del = useDeleteAttachment(pageId)
  const qc = useQueryClient()
  // Refetch when the editor uploads a file (drop/paste) so it appears in the strip.
  useEffect(() => {
    const onChange = (e: Event) => {
      if ((e as CustomEvent<{ pageId: number }>).detail?.pageId === pageId) {
        void qc.invalidateQueries({ queryKey: attachmentKeys.page(pageId) })
      }
    }
    window.addEventListener('tela:attachments-changed', onChange)
    return () => window.removeEventListener('tela:attachments-changed', onChange)
  }, [pageId, qc])
  if (!data || data.length === 0) return null
  return (
    <AttachmentStripView
      attachments={data}
      editable={editable}
      onInsert={onInsert}
      onDelete={editable ? (a) => del.mutate(a.id) : undefined}
      deletingId={del.isPending ? (del.variables as number) : null}
    />
  )
}
