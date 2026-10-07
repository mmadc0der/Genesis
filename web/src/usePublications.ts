import { useCallback, useEffect, useRef, useState } from "react";
import { PAGE_SIZE, fetchPublications, mergeNewer, mergeOlder, newestSeq, type Publication } from "./publications";

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

const POLL_MS = 15000;

// usePublications keeps the front page in step with the register: the newest
// page first, older pages on demand, and newer stories by polling with after.
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

  const refresh = useCallback(() => {
    if (busyNewer.current) return;
    busyNewer.current = true;
    const after = newestSeq(itemsRef.current);
    let restart = after === 0;
    fetchPublications(after > 0 ? { after } : {})
      // A full page of newer stories may have a gap behind it, so start over
      // from the newest page instead of merging.
      .then((page) => {
        if (after > 0 && page.publications.length >= PAGE_SIZE) {
          restart = true;
          return fetchPublications({});
        }
        return page;
      })
      .then((page) => {
        if (!alive.current) return;
        setError(false);
        commit(restart ? page.publications : mergeNewer(itemsRef.current, page.publications));
        if (restart) {
          nextBeforeRef.current = page.next_before;
          setNextBefore(page.next_before);
        }
        setLoaded(true);
      })
      .catch(() => {
        if (alive.current) setError(true);
      })
      .finally(() => {
        busyNewer.current = false;
      });
  }, [commit]);

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
    const timer = window.setInterval(refresh, POLL_MS);
    return () => {
      alive.current = false;
      window.clearInterval(timer);
    };
  }, [refresh]);

  return { items, loaded, error, hasOlder: nextBefore !== undefined, loadingOlder, loadOlder, refresh };
}
