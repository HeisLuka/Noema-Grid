import { lazy, Suspense, useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { Check, Download, ExternalLink, FileText, Link2, Loader2 } from 'lucide-react'
import { usePublicFile } from '../lib/queries/public'
import { PublicShell, PublicUnavailable } from '../components/app/PublicShell'
import { Button } from '../components/ui/button'
import { fileShareUrl } from '../lib/file-link'

// The file page — /f/{hash}/{name}. The link you share for an attachment
// instead of the raw /api/files blob, which forces a download and unfurls as
// nothing. Unauthenticated (child of rootRoute): the blob it shows has always
// been readable by anyone holding its URL, so this page adds a name, a preview
// and a download button on top of exactly that. Crawler UAs never get here —
// Caddy routes them to the backend card (file_page.go).

const PdfDocument = lazy(() => import('../components/ui/pdf-document'))

function prettySize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const kb = bytes / 1024
  if (kb < 1024) return `${kb < 10 ? kb.toFixed(1) : Math.round(kb)} KB`
  const mb = kb / 1024
  return `${mb < 10 ? mb.toFixed(1) : Math.round(mb)} MB`
}

export function FileRoute() {
  const { hash } = useParams({ from: '/f/$hash/{-$name}' })
  const query = usePublicFile(hash)
  const [copied, setCopied] = useState(false)

  if (query.isLoading) {
    return (
      <PublicShell>
        <p role="status" className="m-0 text-[length:var(--text-sm)] text-[var(--text-muted)]">
          Loading…
        </p>
      </PublicShell>
    )
  }
  const file = query.data?.file
  if (query.error || !file) {
    return <PublicUnavailable message="This file isn’t available." />
  }

  const isImage = file.mime.startsWith('image/')
  const isPdf = file.mime === 'application/pdf'
  const copy = async () => {
    if (!navigator.clipboard?.writeText) return
    await navigator.clipboard.writeText(fileShareUrl(file.hash, file.name))
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  return (
    <PublicShell align="stretch">
      <div className="w-full max-w-[64rem] mx-auto flex flex-col min-h-0 gap-[var(--space-4)]">
        <div className="flex items-start justify-between gap-[var(--space-4)] flex-wrap">
          <div className="min-w-0">
            <h2 className="m-0 flex items-center gap-[var(--space-2)] text-[length:var(--text-xl)] leading-[var(--leading-tight)]">
              <FileText width={18} height={18} aria-hidden className="shrink-0 text-[var(--text-muted)]" />
              <span className="truncate">{file.name}</span>
            </h2>
            <p className="mt-[var(--space-1)] mb-0 text-[length:var(--text-sm)] text-[var(--text-muted)]">
              {file.kind} · {prettySize(file.byte_size)}
              {file.page ? (
                <>
                  {' · attached to '}
                  {/* Server-computed canonical path (public_handles.go) —
                      never re-derived client-side, so it stays an <a>. */}
                  <a href={file.page.path} className="underline underline-offset-2">
                    {file.page.title}
                  </a>
                </>
              ) : null}
            </p>
          </div>
          <div className="flex items-center gap-[var(--space-2)]">
            <Button variant="ghost" size="sm" onClick={() => void copy()}>
              {copied ? (
                <Check width={15} height={15} aria-hidden />
              ) : (
                <Link2 width={15} height={15} aria-hidden />
              )}
              {copied ? 'Copied' : 'Copy link'}
            </Button>
            <Button asChild variant="secondary" size="sm">
              <a href={file.url} download={file.name}>
                <Download width={15} height={15} aria-hidden />
                Download
              </a>
            </Button>
          </div>
        </div>

        {isPdf ? (
          // Bounded box: PdfDocument scrolls inside it (`.tela-pdf` is flex:1 of
          // a bounded column), so the page around it still scrolls normally —
          // no fixed shell, nothing to trap a pinch-zoomed touch pan.
          <div className="flex flex-col h-[80dvh] min-h-0">
            <Suspense
              fallback={
                <div className="tela-pdf-status">
                  <Loader2 className="tela-pdf-spin" width={18} height={18} aria-hidden />
                  <span>Loading viewer…</span>
                </div>
              }
            >
              <PdfDocument url={file.url} />
            </Suspense>
          </div>
        ) : isImage ? (
          <img
            src={file.url}
            alt={file.name}
            className="max-w-full rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--surface-2)]"
          />
        ) : (
          <div className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--surface-2)] p-[var(--space-7)] text-center">
            <p className="m-0 text-[length:var(--text-sm)] text-[var(--text-muted)]">
              No preview for {file.kind} files.
            </p>
            <a
              href={file.url}
              target="_blank"
              rel="noopener noreferrer"
              className="mt-[var(--space-3)] inline-flex items-center gap-[var(--space-2)] text-[length:var(--text-sm)] underline underline-offset-2"
            >
              <ExternalLink width={14} height={14} aria-hidden />
              Open the file
            </a>
          </div>
        )}
      </div>
    </PublicShell>
  )
}
