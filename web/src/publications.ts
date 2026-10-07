import type { Edition } from "./editions";

// A publication as GET /api/publications returns it. The register is written
// by the listener and read through control; nothing here is sample data.
export interface Publication {
  seq: number;
  id: string;
  time: string;
  kind: string;
  headline: string;
  lede: string;
  body?: string;
  from: string;
  to?: string;
  run?: string;
  refs?: string[];
  supersedes?: string;
  superseded_by?: string;
}

export interface PublicationPage {
  publications: Publication[];
  next_before?: number;
  last_seq: number;
}

export const PAGE_SIZE = 20;

export function pageQuery(params: { before?: number; after?: number; limit?: number }) {
  const query = new URLSearchParams();
  query.set("limit", String(params.limit ?? PAGE_SIZE));
  if (params.before) query.set("before", String(params.before));
  if (params.after) query.set("after", String(params.after));
  return `/api/publications?${query.toString()}`;
}

export async function fetchPublications(params: { before?: number; after?: number; limit?: number }): Promise<PublicationPage> {
  const response = await fetch(pageQuery(params));
  if (!response.ok) throw new Error(`publications ${response.status}`);
  const page = (await response.json()) as Partial<PublicationPage>;
  return {
    publications: page.publications ?? [],
    next_before: page.next_before,
    last_seq: page.last_seq ?? 0,
  };
}

function bySeqDescending(left: Publication, right: Publication) {
  return right.seq - left.seq;
}

// mergeNewer adds stories that arrived after the ones already shown. A story
// that supersedes a shown one replaces it.
export function mergeNewer(current: Publication[], incoming: Publication[]): Publication[] {
  if (incoming.length === 0) return current;
  const replaced = new Set(incoming.map((item) => item.supersedes).filter((id): id is string => Boolean(id)));
  const known = new Set(current.map((item) => item.id));
  const added = incoming.filter((item) => !known.has(item.id));
  return [...added, ...current.filter((item) => !replaced.has(item.id))].sort(bySeqDescending);
}

// mergeOlder appends the next older page. A story already shown is skipped
// so a page that overlaps the one before it does not repeat.
export function mergeOlder(current: Publication[], incoming: Publication[]): Publication[] {
  if (incoming.length === 0) return current;
  const known = new Set(current.map((item) => item.id));
  const replaced = new Set(current.map((item) => item.supersedes).filter((id): id is string => Boolean(id)));
  const added = incoming.filter((item) => !known.has(item.id) && !replaced.has(item.id));
  return [...current, ...added].sort(bySeqDescending);
}

export function newestSeq(items: Publication[]): number {
  return items.reduce((max, item) => Math.max(max, item.seq), 0);
}

export function formatStoryTime(iso: string, now: number): string {
  const at = Date.parse(iso);
  if (Number.isNaN(at)) return iso;
  const minutes = Math.max(0, Math.round((now - at) / 60000));
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} h ago`;
  return new Date(at).toLocaleString("en", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function toEdition(item: Publication, now: number): Edition {
  const byline = [item.from];
  if (item.to) byline.push(`to ${item.to}`);
  if (item.run) byline.push(item.run);
  return {
    id: item.id,
    time: formatStoryTime(item.time, now),
    category: item.kind,
    headline: item.headline,
    lede: item.lede,
    body: item.body,
    byline,
  };
}
