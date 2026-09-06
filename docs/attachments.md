# Attachments

Files attached to a page — uploads from the editor **and** files
[rclone-synced](webdav-sync.md) into the page's folder — backed by the unified
`space_files` blob store (migration `0015`). The same store backs editor image
uploads.

## Model

A page's attachments are the `space_files` whose `parent_page_id` is that page.
A file dropped into `pageX/` over WebDAV is already parented to `pageX`, so
synced files become attachments with **no body edit** and no sync-layer rewrite
of the markdown.

- **Inline** placement lives in the body (markdown): images as `![](url)`,
  other files as a `:::file{name="…" size="…"}\n<url>\n:::` card
  (`milkdown-file.ts`, block id `file`). Authoritative position, syncs as text.
- **The Attachments strip** (below the title, reader + editor) is a sidecar
  rendered from SQL — it lists *all* files on the page. A chip carries an
  "embedded" marker when the body already references that file's hash (computed
  by scanning the body, stateless). In the editor a chip can be deleted.

## API

- `GET  /api/pages/{id}/attachments` — session-authed; lists the page's files
  with `{id, name, mime, byte_size, hash, url, embedded}`.
- `POST /api/pages/{id}/attachments` — editor+; multipart `file`. Dedupes
  identical bytes; disambiguates a name collision with a `-<hash8>` suffix so a
  distinct upload never clobbers an existing one (e.g. two pasted `image.png`).
- `DELETE /api/pages/{id}/attachments/{file_id}` — editor+; soft-delete.
- `GET /api/files/{space_id}/{hash}.{ext}` — **public**, content-addressed,
  immutable cache. Keyed by hash (not path) so a body embed survives a sync
  rename. Raster images (png/jpeg/gif/webp) serve **inline**; everything else is
  forced to download (`Content-Disposition: attachment` + `nosniff`) so embedded
  HTML/SVG can't execute as stored-XSS from our origin.

The list/upload/delete handlers are thin wrappers over `listPageAttachmentsCore`
/ `uploadPageAttachmentCore` / `deletePageAttachmentCore` — the same cores the
MCP tools call, so REST and agents share one code path.

## Sharing a file

`/api/files/…` is the BLOB. It forces a download, its name is a 64-hex hash, and
it unfurls as nothing — pasted in Slack or WhatsApp it is a bare link. So it is
not what a person should be handing to another person.

**`/f/{hash12}/{filename}`** is (`api/file_page.go` + `routes/file.tsx`). It
resolves any 8–64 hex *prefix* of the content hash — the filename segment is
decorative, so a rename never breaks a link already shared — and renders the file
in tela chrome: the PDF viewer for PDFs, the image inline, a download card
otherwise, plus Copy link / Download and (public spaces only) a link back to the
page it is attached to. The Attachments strip's chip copies THIS url
(`lib/file-link.ts`); nothing in the UI hands out the blob URL any more.

Bot-gated exactly like `/share` and `/public`: Caddy routes crawler UAs to the
backend, which answers with an OG envelope + a `/f/{hash}/og.png` card (filename
as the title, `PDF · 46 KB` under it); humans fall through to the SPA at the same
path. The handler 404s a non-bot as defence in depth, so a missing Caddy gate is
visible instead of serving the crawler envelope in place of the app.

**The retrofit:** a crawler UA asking for a *non-image* `/api/files/…` blob gets
that same card instead of the bytes, so the thousands of blob URLs already pasted
somewhere unfurl too. Raster images are exempt — page bodies embed them and a
fetcher asking for an image wants the pixels.

**Disclosure.** The card carries filename, type and size — exactly what the
blob's own `Content-Disposition` already tells anyone holding the URL. The parent
page's title, and the file's auto-summary, appear ONLY when the owning space is
public; a private space's file unfurls as "PDF · 46 KB · shared from tela".

Adding a URL shape here means updating BOTH `deploy/proxy/sites.caddy` and
`frontend/nginx.conf` — nginx serves the SPA shell as its 404 body, so a path
missing from its allowlist renders perfectly while returning 404.

## MCP tools

Agents get the same surface (put an image/PDF on a page, or read what's
attached):

- `list_attachments(page_id)` — read; each file plus an absolute `download_url`
  (fetchable over HTTP) and a ready-to-paste `markdown` embed snippet.
- `upload_attachment(page_id, name, data_base64)` — editor+; stores the bytes
  (inline base64) and returns the attachment + `markdown`. The agent then
  `update_page`/`patch_page`s the snippet into the body — `![](…)` for images, a
  `:::file` card otherwise. **Capped at 5 MB** (`mcpInlineUploadCap`): the base64
  rides through the model's context, so an oversize blob bloats tokens and can
  trip a host content filter — over the cap it 413s and points to the handshake.
- `delete_attachment(page_id, id)` — editor+; soft-delete. Does **not** edit the
  body, so an inline embed must be removed separately.

### Two upload tiers (binary over MCP)

MCP has **no upload primitive** (resources are server→client only), so servers
improvise; we follow the de-facto two-tier convention (cf. Notion's File Upload
API, Scenario MCP):

1. **Inline base64** — `upload_attachment`, ≤ 5 MB. Works on every host. The
   common case (a screenshot, a chart, a short PDF). The cap is the *transport*
   limit; `TELA_WEBDAV_FILE_MAX_BYTES` (50 MiB) stays the storage limit.
2. **Signed-PUT handshake** (`attachment_uploads.go`, migration `0035`) — for
   larger files, so the bytes never touch the model context:
   - `request_attachment_upload(page_id, name, mime)` → a short-TTL (5 min) HMAC
     `put_url` (share-secret signed, like the PDF print token) + an `upload_id`.
   - the host `PUT`s the raw bytes to `put_url` (public `PUT /api/uploads/{token}`,
     self-authenticating on `auth.IsPublicPath`, single-use, size-capped) → stored
     content-addressed into `space_files`, parented to the page; the PUT response
     carries the attachment ref.
   - `confirm_attachment_upload(upload_id)` → returns the ref + `markdown` for
     hosts that couldn't read the PUT response; then embed with `update_page`.
   - Only usable where the host can make an outbound HTTP PUT (a code sandbox);
     otherwise inline base64 is the universal fallback. The `attachment_uploads`
     row maps `upload_id → space_file`; rows are swept (>24h) on the next request.

Download needs no upload tier: `download_url` is a public URL the host fetches
directly (the MCP guidance for web-fetchable resources). PDF attachments render
inline in the reader via a client-side viewer (`ui/pdf-viewer.tsx`).

## Notes

- **Serve identity is a capability hash.** Like `/api/images/`, anyone with the
  URL can fetch the bytes regardless of space privacy — which is why the file
  page above is public too, and why its card stops at what the blob already
  discloses. Fine for most wikis; if
  strict private-space enforcement is needed, gate the serve route on visibility
  (costs the immutable-cache win).
- **Storage** is Postgres `bytea`, capped per file by `TELA_WEBDAV_FILE_MAX_BYTES`
  (default 50 MiB). It bloats DB/backups; the serve-route abstraction lets blobs
  move to filesystem/S3 later without changing URLs.
- **Legacy image serve** (`/api/images/`) is now **serve-only** — the old
  `POST /api/pages/{id}/images` upload route + `UploadPageImage` were retired
  (the editor uploads everything through `/api/pages/{id}/attachments`). The GET
  route + `page_images` table stay so images already embedded in historical page
  bodies keep resolving; dropping the table would need a body-URL rewrite + data
  migration first. `/api/diagrams/` is unchanged.
- **Conflicts** on a file are last-write-wins (see [webdav-sync.md](webdav-sync.md));
  no per-file history.
