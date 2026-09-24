import { useState } from "preact/hooks";
import { activityPreview, type ChatLine } from "./model";

export function ActivityLine({ line }: { line: ChatLine }) {
  const [open, setOpen] = useState(false);
  return (
    <button
      type="button"
      class={`line line-${line.kind}${open ? " open" : ""}`}
      aria-expanded={open}
      onClick={() => setOpen((current) => !current)}
    >
      <span class="kind">{line.kind}</span>
      <time>{line.time || ""}</time>
      {open ? <p class="full">{line.text}</p> : <p class="preview">{activityPreview(line)}</p>}
    </button>
  );
}
