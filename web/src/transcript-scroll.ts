import { useLayoutEffect, useRef } from "preact/hooks";

export const TRANSCRIPT_STICK_THRESHOLD_PX = 40;

export function transcriptSticksToBottom(
  scrollTop: number,
  scrollHeight: number,
  clientHeight: number,
  threshold = TRANSCRIPT_STICK_THRESHOLD_PX,
): boolean {
  return scrollHeight - scrollTop - clientHeight <= threshold;
}

export function shouldFollowTranscript(input: {
  runChanged: boolean;
  previouslyFollowing: boolean;
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}): boolean {
  if (input.runChanged) return true;
  if (input.clientHeight <= 0) return input.previouslyFollowing;
  return transcriptSticksToBottom(input.scrollTop, input.scrollHeight, input.clientHeight);
}

// Sample the scrollport during render, while the DOM still has the previous
// lines. After commit, a pinned reader is no longer within the threshold.
export function useTranscriptFollow(
  transcriptRef: { current: HTMLDivElement | null },
  events: unknown,
  selectedRun: string,
): void {
  const followRef = useRef(true);
  const followRunRef = useRef(selectedRun);
  const node = transcriptRef.current;
  followRef.current = shouldFollowTranscript({
    runChanged: followRunRef.current !== selectedRun,
    previouslyFollowing: followRef.current,
    scrollTop: node?.scrollTop ?? 0,
    scrollHeight: node?.scrollHeight ?? 0,
    clientHeight: node?.clientHeight ?? 0,
  });

  useLayoutEffect(() => {
    followRunRef.current = selectedRun;
    const el = transcriptRef.current;
    if (!el || !followRef.current) return;
    el.scrollTop = el.scrollHeight;
  }, [events, selectedRun, transcriptRef]);
}
