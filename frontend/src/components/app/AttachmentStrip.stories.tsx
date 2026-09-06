import type { Meta, StoryObj } from '@storybook/react-vite'
import { AttachmentStripView } from './AttachmentStrip'
import type { Attachment } from '../../lib/queries/attachments'

// The presentational strip — fed fixture attachments so the story needs no
// query client or backend. Mirrors what the data container (AttachmentStrip)
// renders once useAttachments resolves.

const meta: Meta<typeof AttachmentStripView> = {
  title: 'App/AttachmentStrip',
  component: AttachmentStripView,
}
export default meta
type Story = StoryObj<typeof AttachmentStripView>

function file(
  id: number,
  name: string,
  mime: string,
  byte_size: number,
  embedded = false,
  summary?: string,
): Attachment {
  return {
    id,
    name,
    mime,
    byte_size,
    hash: `${id}`.repeat(8),
    url: '#',
    share_path: `/f/${`${id}`.repeat(12)}/${name}`,
    embedded,
    summary,
  }
}

const mixed: Attachment[] = [
  file(
    1,
    'q3-planning.pdf',
    'application/pdf',
    248_000,
    false,
    'Q3 roadmap: ships the billing rework and the new onboarding flow, with hiring targets for two backend engineers.',
  ),
  file(2, 'architecture.png', 'image/png', 91_000, true),
  file(3, 'budget.xlsx', 'application/vnd.ms-excel', 18_400),
  file(4, 'notes.txt', 'text/plain', 1_200),
]

// Default landing state: a quiet count toggle, not the full chip row.
export const Collapsed: Story = {
  args: { attachments: mixed },
}

export const ReadOnly: Story = {
  args: { attachments: mixed, defaultOpen: true },
}

export const Editable: Story = {
  args: { attachments: mixed, editable: true, defaultOpen: true },
}

export const ManyExpanded: Story = {
  args: {
    defaultOpen: true,
    attachments: Array.from({ length: 14 }, (_, i) =>
      file(i + 1, `document-${i + 1}.pdf`, 'application/pdf', 10_000 * (i + 1)),
    ),
  },
}

export const Empty: Story = {
  args: { attachments: [] },
}
