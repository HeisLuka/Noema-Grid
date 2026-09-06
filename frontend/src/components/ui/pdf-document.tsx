import { useCallback, useEffect, useRef, useState } from 'react'
import { Document, Page, pdfjs } from 'react-pdf'
import { Loader2, ZoomIn, ZoomOut } from 'lucide-react'
import 'react-pdf/dist/Page/TextLayer.css'
import 'react-pdf/dist/Page/AnnotationLayer.css'

// The heavy half of the PDF viewer: react-pdf + the pdf.js engine. Kept in its
// own module so consumers can `lazy(() => import('./pdf-document'))` and the
// pdf.js bundle is code-split out of the main app — it only loads when someone
// actually opens a preview. Renders the bytes client-side (pdf.js fetches the
// serve URL directly), so the backend keeps forcing `Content-Disposition:
// attachment` on PDFs — nothing executes from our origin.

// Worker is bundled as an asset by Vite via import.meta.url, so it stays
// version-locked to the installed pdfjs-dist.
//
// The `?v=2` busts POISONED CLIENT CACHES. Until the nginx fix, this .mjs was
// served as application/octet-stream (nginx's mime.types has no .mjs) and the
// browser refused it as a module script, so the viewer failed with "Couldn't
// display this PDF". Two things make that stick: /assets/ is
// `immutable, max-age=1y` and the filename hash only moves when pdfjs-dist
// does, AND a hard reload does not reliably refetch a script a worker pulls in
// later — so an affected browser keeps the broken copy for a YEAR and looks
// unfixable from the user's side. A different query is a different cache key:
// one clean refetch, everywhere, without touching the vendored file.
pdfjs.GlobalWorkerOptions.workerSrc =
  new URL('pdfjs-dist/build/pdf.worker.min.mjs', import.meta.url).toString() +
  '?v=2'

const ZOOM_MIN = 0.5
const ZOOM_MAX = 3
const ZOOM_STEP = 0.25

function PdfStatus({
  children,
  spin,
}: {
  children: React.ReactNode
  spin?: boolean
}) {
  return (
    <div className="tela-pdf-status">
      {spin ? (
        <Loader2 className="tela-pdf-spin" width={18} height={18} aria-hidden />
      ) : null}
      <span>{children}</span>
    </div>
  )
}

export default function PdfDocument({ url }: { url: string }) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const [baseWidth, setBaseWidth] = useState(0)
  const [numPages, setNumPages] = useState(0)
  const [scale, setScale] = useState(1)

  // Fit a page to the scroll container, re-measuring on resize; zoom multiplies
  // that base so the canvas never overflows the viewport at 100%.
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const measure = () => setBaseWidth(Math.max(0, el.clientWidth - 48))
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const zoom = useCallback(
    (dir: 1 | -1) =>
      setScale((s) =>
        Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, +(s + dir * ZOOM_STEP).toFixed(2))),
      ),
    [],
  )

  return (
    <div className="tela-pdf">
      <div className="tela-pdf-toolbar">
        <span>
          {numPages ? `${numPages} page${numPages > 1 ? 's' : ''}` : ' '}
        </span>
        <span className="tela-pdf-zoom">
          <button
            type="button"
            onClick={() => zoom(-1)}
            disabled={scale <= ZOOM_MIN}
            aria-label="Zoom out"
          >
            <ZoomOut width={15} height={15} aria-hidden />
          </button>
          <span>{Math.round(scale * 100)}%</span>
          <button
            type="button"
            onClick={() => zoom(1)}
            disabled={scale >= ZOOM_MAX}
            aria-label="Zoom in"
          >
            <ZoomIn width={15} height={15} aria-hidden />
          </button>
        </span>
      </div>
      <div className="tela-pdf-scroll" ref={scrollRef}>
        <Document
          file={url}
          onLoadSuccess={({ numPages }) => setNumPages(numPages)}
          loading={<PdfStatus spin>Loading PDF…</PdfStatus>}
          error={
            <PdfStatus>
              Couldn’t display this PDF — use Download to open it.
            </PdfStatus>
          }
          noData={<PdfStatus>No PDF to show.</PdfStatus>}
        >
          {baseWidth > 0
            ? Array.from({ length: numPages }, (_, i) => (
                <Page
                  key={i}
                  pageNumber={i + 1}
                  width={baseWidth * scale}
                  className="tela-pdf-page"
                  loading=""
                  renderTextLayer
                  renderAnnotationLayer
                />
              ))
            : null}
        </Document>
      </div>
    </div>
  )
}
