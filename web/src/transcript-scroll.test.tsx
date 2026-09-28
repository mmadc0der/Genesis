// @vitest-environment happy-dom
import { render } from "preact";
import { useRef } from "preact/hooks";
import { afterEach, describe, expect, it } from "vitest";
import { shouldFollowTranscript, transcriptSticksToBottom, useTranscriptFollow } from "./transcript-scroll";

describe("transcript stickiness", () => {
  it("sticks within 40px of the bottom", () => {
    expect(transcriptSticksToBottom(0, 140, 100)).toBe(true);
    expect(transcriptSticksToBottom(80, 200, 120)).toBe(true);
    expect(transcriptSticksToBottom(200, 200, 40)).toBe(true);
    expect(transcriptSticksToBottom(0, 141, 100)).toBe(false);
  });

  it("uses the position from before appended lines grow the scroll height", () => {
    const pinned = { scrollTop: 160, scrollHeight: 200, clientHeight: 40 };
    expect(shouldFollowTranscript({ runChanged: false, previouslyFollowing: true, ...pinned })).toBe(true);
    const afterGrowth = { scrollTop: 160, scrollHeight: 280, clientHeight: 40 };
    expect(transcriptSticksToBottom(afterGrowth.scrollTop, afterGrowth.scrollHeight, afterGrowth.clientHeight)).toBe(false);
  });

  it("stops following when the reader scrolls up", () => {
    expect(shouldFollowTranscript({
      runChanged: false,
      previouslyFollowing: true,
      scrollTop: 0,
      scrollHeight: 800,
      clientHeight: 120,
    })).toBe(false);
  });

  it("follows again after the reader returns to the bottom", () => {
    expect(shouldFollowTranscript({
      runChanged: false,
      previouslyFollowing: false,
      scrollTop: 640,
      scrollHeight: 800,
      clientHeight: 120,
    })).toBe(true);
  });

  it("starts at the bottom when the selected run changes", () => {
    expect(shouldFollowTranscript({
      runChanged: true,
      previouslyFollowing: false,
      scrollTop: 0,
      scrollHeight: 800,
      clientHeight: 120,
    })).toBe(true);
  });

  it("keeps the previous decision until the scrollport has a height", () => {
    expect(shouldFollowTranscript({
      runChanged: false,
      previouslyFollowing: false,
      scrollTop: 0,
      scrollHeight: 0,
      clientHeight: 0,
    })).toBe(false);
    expect(shouldFollowTranscript({
      runChanged: false,
      previouslyFollowing: true,
      scrollTop: 0,
      scrollHeight: 0,
      clientHeight: 0,
    })).toBe(true);
  });
});

function Host({ lines, run }: { lines: string[]; run: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useTranscriptFollow(ref, lines, run);
  return (
    <div id="transcript" ref={ref}>
      {lines.map((line) => (
        <p key={line}>{line}</p>
      ))}
    </div>
  );
}

function installMetrics(el: HTMLDivElement) {
  const clientHeight = 50;
  let scrollTop = 0;
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => clientHeight });
  Object.defineProperty(el, "scrollHeight", {
    configurable: true,
    get: () => 100 + el.childElementCount * 80,
  });
  Object.defineProperty(el, "scrollTop", {
    configurable: true,
    get: () => scrollTop,
    set: (value: number) => {
      scrollTop = value;
    },
  });
  return {
    clientHeight,
    get scrollTop() {
      return scrollTop;
    },
    set scrollTop(value: number) {
      scrollTop = value;
    },
    scrollHeight: () => 100 + el.childElementCount * 80,
  };
}

function mount(lines: string[], run: string) {
  const host = document.createElement("div");
  document.body.append(host);
  render(<Host lines={lines} run={run} />, host);
  const el = host.querySelector("#transcript");
  if (!(el instanceof HTMLDivElement)) throw new Error("missing transcript");
  return { host, el, metrics: installMetrics(el) };
}

afterEach(() => {
  document.body.replaceChildren();
});

describe("transcript follow", () => {
  it("follows new lines only when the reader was already at the bottom", () => {
    const { host, metrics } = mount(["one"], "run-1");
    metrics.scrollTop = metrics.scrollHeight() - metrics.clientHeight;

    render(<Host lines={["one", "two"]} run="run-1" />, host);
    expect(metrics.scrollTop).toBe(metrics.scrollHeight());

    metrics.scrollTop = 0;
    render(<Host lines={["one", "two", "three"]} run="run-1" />, host);
    expect(metrics.scrollTop).toBe(0);

    const stuck = metrics.scrollHeight() - metrics.clientHeight - 40;
    metrics.scrollTop = stuck;
    render(<Host lines={["one", "two", "three", "four"]} run="run-1" />, host);
    expect(metrics.scrollTop).toBe(metrics.scrollHeight());

    const released = metrics.scrollHeight() - metrics.clientHeight - 41;
    metrics.scrollTop = released;
    render(<Host lines={["one", "two", "three", "four", "five"]} run="run-1" />, host);
    expect(metrics.scrollTop).toBe(released);
  });

  it("jumps to the bottom when the selected run changes, then follows until the reader scrolls up", () => {
    const { host, metrics } = mount(["one"], "run-1");
    metrics.scrollTop = 0;

    render(<Host lines={["one", "two"]} run="run-2" />, host);
    expect(metrics.scrollTop).toBe(metrics.scrollHeight());

    render(<Host lines={["one", "two", "three"]} run="run-2" />, host);
    expect(metrics.scrollTop).toBe(metrics.scrollHeight());

    metrics.scrollTop = 0;
    render(<Host lines={["one", "two", "three", "four"]} run="run-2" />, host);
    expect(metrics.scrollTop).toBe(0);
  });
});
