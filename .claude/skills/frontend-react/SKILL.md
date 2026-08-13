---
name: frontend-react
description: Use when working in seera-web — React components, TypeScript types, Tailwind styling, state management, API/SSE integration, loading and error states, citation UI, document viewer, accessibility, or responsive layout.
---

# Frontend (React + TypeScript + Vite)

React 19 with the React Compiler enabled, TypeScript, Vite 8, Tailwind for
styling. SPA — no Next.js, no SSR (see `project-architecture`).

## Structure

```
src/
  app/           router, providers, layout shell
  features/
    chat/        ask flow, streaming answer, citation rendering
    documents/   browser, viewer, highlighting
    search/      filters, results
  components/    reusable presentational primitives
  lib/           api client, sse client, formatting, types
  hooks/
```

Feature-first. A component used by exactly one feature lives in that feature.
Promote to `components/` on the second real consumer, not in anticipation.

## Components

- Function components, named exports, one component per file.
- Props typed with an explicit `type`. No `any`, no `React.FC`.
- Presentational components take data and callbacks; they don't fetch.
- Container/route components own data fetching and pass results down.
- Compose over configure: prefer `<Card><CardHeader/></Card>` to a `variant`
  prop that grew twelve options.
- React Compiler is on — do not hand-add `useMemo`/`useCallback`/`memo` unless
  a profile shows a real problem. Keep components pure.

## State

Four tiers, use the smallest that works:

1. `useState` — local UI state.
2. URL search params — anything shareable or reloadable: query, filters, active
   document, active citation. The address bar is state.
3. Server cache (TanStack Query when added) — fetched data, with `queryKey`
   including every input that changes the result.
4. Context — cross-cutting only (auth, theme, locale). Never a global app store
   holding server data.

No Redux. No Zustand until there is state that genuinely fails all four tiers.

## API layer

All network access goes through `lib/api/`. Components never call `fetch`.

```ts
export type Citation = {
  chunkId: string
  documentId: string
  documentTitle: string
  articleNo: string | null
  sectionPath: string
  effectiveFrom: string
  snippet: string
}
```

Types mirror the Go DTOs. Validate responses at the boundary (zod) rather than
casting — a schema change should fail loudly in one place.

## SSE client

The ask endpoint streams (`sources`, `token`, `citation`, `done`, `error`).

- Use `fetch` + `ReadableStream` rather than `EventSource` — the request is a
  POST with a JSON body and auth headers.
- Parse the SSE framing incrementally; a chunk can split an event mid-frame.
  Buffer until `\n\n`.
- Render sources as soon as the `sources` event lands, before tokens arrive.
- Append tokens to a single state string; do not re-render per-character with
  a new array.
- Abort on unmount and on user cancel via `AbortController`.
- Handle the `error` event and abrupt stream close distinctly: one is a server
  error with a code, the other is a connection failure that offers retry.
- Never leave the UI in a permanent "thinking" state — every terminal path
  (`done`, `error`, abort, network failure) must clear the streaming flag.

## Loading and error states

Every async surface implements four states explicitly: idle, loading, error,
empty. "Empty" for legal search is a first-class result — the system abstaining
because nothing relevant was found is not an error, and must not be styled as
one. Show what was searched and how to narrow.

Skeletons that match the final layout, not spinners. Errors are actionable:
what failed, what to do, a retry control.

## Citation UI

Citations are the trust surface. Requirements:

- Inline markers in the answer are interactive: hover previews the source
  snippet, click opens the document viewer at that span.
- Every citation shows document title, article/section, and effective date.
- A sources panel lists all grounding chunks for the answer, in rank order.
- Render citations from the structured `citations` array in the response —
  never by regex-parsing the answer text.
- If the answer abstains, say so plainly; do not render a sources panel that
  implies grounding that isn't there.

## Document viewer

- Paginated rendering; virtualize long documents.
- Deep-linkable: `/documents/:id?version=&article=&chunk=`.
- Scroll-to and highlight the cited span using `char_start`/`char_end` from the
  API.
- Show the version and effective date prominently, with a control to view other
  versions. A user must never be unsure which version they are reading.

## Accessibility

Non-negotiable for a legal product:

- Semantic HTML first; ARIA only when no element fits.
- Streaming answer region is `aria-live="polite"` — announce completion, not
  every token.
- All interactive elements keyboard-reachable with visible focus rings.
- Citation markers are `<button>`s with labels like "Source 3: Proclamation
  1051/2017, Article 27" — not bare superscript numbers.
- WCAG AA contrast. Never color alone to convey meaning.
- Modals: focus trap, `Esc` closes, focus restored on close.
- Amharic content: `lang="am"` on those nodes, and verify font rendering of
  Ethiopic script.

## Responsive

Mobile-first Tailwind breakpoints. On narrow screens the answer and the document
viewer are separate views (tabs or a sheet), not a squeezed split pane. Tap
targets ≥44px. Test the citation flow at 375px width — it is the hardest layout.

## Tailwind

Design tokens in the theme config; no arbitrary hex values in components.
Extract a component when a class list repeats, not a `@apply` soup. Keep class
order consistent (layout → box → typography → color → state).
