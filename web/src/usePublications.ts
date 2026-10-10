import { useCallback, useEffect, useRef, useState } from "react";
import { onLive } from "./liveSocket";
import { PAGE_SIZE, fetchPublications, mergeNewer, mergeOlder, type Publication } from "./publications";

export interface PublicationFeed {
  items: Publication[];
  // loaded is true once the first page has answered, empty or not.
  loaded: boolean;
  error: boolean;
  hasOlder: boolean;
  loadingOlder: boolean;
  loadOlder: () => void;
  refresh: () => void;
}

// usePublications keeps the front page in step with the register: the newest
// page first, older pages on demand, and newer stories pushed on /api/live.
export function usePublications(): PublicationFeed {
  const [items, setItems] = useState<Publication[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState(false);
  const [nextBefore, setNextBefore] = useState<number | undefined>(undefined);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const itemsRef = useRef<Publication[]>([]);
  const nextBeforeRef = useRef<number | undefined>(undefined);
  const busyOlder = useRef(false);
  const busyNewer = useRef(false);
  const alive = useRef(true);

  const commit = useCallback((next: Publication[]) => {
    itemsRef.current = next;
    setItems(next);
  }, []);

  const applyPage = useCallback((page: { publications: Publication[]; next_before?: number }, replace: boolean) => {
    setError(false);
    commit(replace ? page.publications : mergeNewer(itemsRef.current, page.publications));
    if (replace) {
      nextBeforeRef.current = page.next_before;
      setNextBefore(page.next_before);
    }
    setLoaded(true);
  }, [commit]);

  const refresh = useCallback(() => {
    if (busyNewer.current) return;
    busyNewer.current = true;
    fetchPublications({})
      .then((page) => {
        if (!alive.current) return;
        applyPage(page, true);
      })
      .catch(() => {
        if (alive.current) setError(true);
      })
      .finally(() => {
        busyNewer.current = false;
      });
  }, [applyPage]);

  const loadOlder = useCallback(() => {
    const before = nextBeforeRef.current;
    if (before === undefined || busyOlder.current) return;
    busyOlder.current = true;
    setLoadingOlder(true);
    fetchPublications({ before })
      .then((page) => {
        if (!alive.current) return;
        setError(false);
        commit(mergeOlder(itemsRef.current, page.publications));
        nextBeforeRef.current = page.next_before;
        setNextBefore(page.next_before);
      })
      .catch(() => {
        if (alive.current) setError(true);
      })
      .finally(() => {
        busyOlder.current = false;
        if (alive.current) setLoadingOlder(false);
      });
  }, [commit]);

  useEffect(() => {
    alive.current = true;
    refresh();
    const unlisten = onLive((frame) => {
      if (frame.op !== "publications" || !frame.publications) return;
      const page = frame.publications;
      if (page.publications.length >= PAGE_SIZE && !frame.snapshot && !frame.restart) {
        refresh();
        return;
      }
      applyPage(page, Boolean(frame.snapshot || frame.restart));
    });
    return () => {
      alive.current = false;
      unlisten();
    };
  }, [applyPage, refresh]);

  return { items, loaded, error, hasOlder: nextBefore !== undefined, loadingOlder, loadOlder, refresh };
}
