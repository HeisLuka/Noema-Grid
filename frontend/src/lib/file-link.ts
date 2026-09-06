// The shareable link for a stored file: /f/{hash12}/{name}.
//
// /api/files/{space}/{hash}.{ext} is the BLOB — it downloads, unfurls as
// nothing and reads as gibberish in a chat. This is what a "copy link" hands
// over instead: a short content hash plus the real filename, resolving to the
// file page (routes/file.tsx) that previews it and gives crawlers a card.
//
// The name is decorative — the hash resolves the file — so a rename never
// breaks a link already shared. FILE_HASH_SHORT mirrors fileHashShortLen in
// backend/internal/api/file_page.go; the backend resolves any 8–64 hex prefix,
// so the two only have to agree on what looks nice, not on correctness.
export const FILE_HASH_SHORT = 12

export function fileSharePath(hash: string, name?: string | null): string {
  const p = `/f/${hash.slice(0, FILE_HASH_SHORT)}`
  return name ? `${p}/${encodeURIComponent(name)}` : p
}

export function fileShareUrl(hash: string, name?: string | null): string {
  return `${window.location.origin}${fileSharePath(hash, name)}`
}
