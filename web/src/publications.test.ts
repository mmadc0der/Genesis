import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchPublications,
  formatStoryTime,
  mergeNewer,
  mergeOlder,
  newestSeq,
  pageQuery,
  toEdition,
  type Publication,
} from "./publications";

function story(seq: number, extra: Partial<Publication> = {}): Publication {
  return {
    seq,
    id: `pub_${String(seq).padStart(24, "0")}`,
    time: "2026-10-06T12:00:00Z",
    kind: "report",
    headline: `Story ${seq}`,
    lede: "Lede.",
    from: "oracle",
    ...extra,
  };
}

describe("pageQuery", () => {
  it("asks for the newest page by default", () => {
    expect(pageQuery({})).toBe("/api/publications?limit=20");
  });

  it("carries the paging cursors", () => {
    expect(pageQuery({ before: 41 })).toBe("/api/publications?limit=20&before=41");
    expect(pageQuery({ after: 7, limit: 5 })).toBe("/api/publications?limit=5&after=7");
  });
});

describe("mergeOlder", () => {
  it("appends older stories newest first without repeating one", () => {
    const shown = [story(9), story(8)];
    const merged = mergeOlder(shown, [story(8), story(7), story(6)]);
    expect(merged.map((item) => item.seq)).toEqual([9, 8, 7, 6]);
  });

  it("does not bring back a story that a shown one replaces", () => {
    const replacement = story(9, { supersedes: story(5).id });
    const merged = mergeOlder([replacement], [story(6), story(5)]);
    expect(merged.map((item) => item.seq)).toEqual([9, 6]);
  });

  it("returns the same list for an empty page", () => {
    const shown = [story(3)];
    expect(mergeOlder(shown, [])).toBe(shown);
  });
});

describe("mergeNewer", () => {
  it("puts new stories in front", () => {
    const merged = mergeNewer([story(3), story(2)], [story(5), story(4)]);
    expect(merged.map((item) => item.seq)).toEqual([5, 4, 3, 2]);
  });

  it("removes the story a new one supersedes", () => {
    const merged = mergeNewer([story(3), story(2)], [story(4, { supersedes: story(2).id })]);
    expect(merged.map((item) => item.seq)).toEqual([4, 3]);
  });

  it("ignores a story that is already shown", () => {
    const merged = mergeNewer([story(3)], [story(3)]);
    expect(merged.map((item) => item.seq)).toEqual([3]);
  });
});

describe("newestSeq", () => {
  it("is zero for an empty register and the highest seq otherwise", () => {
    expect(newestSeq([])).toBe(0);
    expect(newestSeq([story(2), story(9), story(4)])).toBe(9);
  });
});

describe("formatStoryTime", () => {
  const now = Date.parse("2026-10-06T12:00:00Z");
  it("speaks in minutes and hours, then dates", () => {
    expect(formatStoryTime("2026-10-06T11:59:50Z", now)).toBe("just now");
    expect(formatStoryTime("2026-10-06T11:15:00Z", now)).toBe("45 min ago");
    expect(formatStoryTime("2026-10-06T09:00:00Z", now)).toBe("3 h ago");
    expect(formatStoryTime("2026-09-01T09:00:00Z", now)).not.toMatch(/ago/);
  });

  it("keeps text it cannot parse", () => {
    expect(formatStoryTime("yesterday", now)).toBe("yesterday");
  });
});

describe("toEdition", () => {
  it("builds the byline from the author, recipient, and run", () => {
    const now = Date.parse("2026-10-06T12:00:00Z");
    const edition = toEdition(story(1, { to: "writer", run: "gen_abc", body: "text" }), now);
    expect(edition.byline).toEqual(["oracle", "to writer", "gen_abc"]);
    expect(edition.category).toBe("report");
    expect(edition.body).toBe("text");
  });
});

describe("fetchPublications", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("reads a page from the control API", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ publications: [story(2)], next_before: 2, last_seq: 4 }),
    });
    vi.stubGlobal("fetch", fetchMock);
    const page = await fetchPublications({ before: 9 });
    expect(fetchMock).toHaveBeenCalledWith("/api/publications?limit=20&before=9");
    expect(page.next_before).toBe(2);
    expect(page.last_seq).toBe(4);
    expect(page.publications).toHaveLength(1);
  });

  it("treats a missing list as an empty register", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) }));
    const page = await fetchPublications({});
    expect(page).toEqual({ publications: [], next_before: undefined, last_seq: 0 });
  });

  it("fails on a refused request", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 500 }));
    await expect(fetchPublications({})).rejects.toThrow("publications 500");
  });
});
