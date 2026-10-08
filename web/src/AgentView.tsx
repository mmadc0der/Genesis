import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Markdown } from "./Markdown";

export interface FormField {
  name: string;
  type: "string" | "integer" | "boolean" | "array" | "map" | "object";
  required?: boolean;
  enum?: string[];
  format?: string;
  description?: string;
  items?: string;
  fields?: FormField[];
}

type Doc = Record<string, unknown>;

export function AgentView({ agentId, onBack }: { agentId: string; onBack: () => void }) {
  const [fields, setFields] = useState<FormField[]>([]);
  const [doc, setDoc] = useState<Doc | null>(null);
  const [presence, setPresence] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let stopped = false;
    setDoc(null);
    setError("");
    setNote("");
    Promise.all([
      fetch("/api/agent-schema").then((response) => response.json() as Promise<{ fields: FormField[] }>),
      fetch(`/api/agents/${encodeURIComponent(agentId)}`).then(async (response) => {
        if (!response.ok) throw new Error(`Agent ${response.status}`);
        return response.json() as Promise<{ presence?: string; document?: Doc }>;
      }),
    ])
      .then(([schema, agent]) => {
        if (stopped) return;
        setFields(schema.fields ?? []);
        setDoc(agent.document ?? {});
        setPresence(agent.presence ?? "");
      })
      .catch((failure: unknown) => {
        if (!stopped) setError(failure instanceof Error ? failure.message : "Control did not answer.");
      });
    return () => {
      stopped = true;
    };
  }, [agentId]);

  async function save() {
    if (!doc || saving) return;
    setSaving(true);
    setError("");
    setNote("");
    const response = await fetch(`/api/agents/${encodeURIComponent(agentId)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ document: doc }),
    });
    const text = await response.text();
    setSaving(false);
    if (!response.ok) {
      setError(text || `Save failed (${response.status})`);
      return;
    }
    const saved = JSON.parse(text) as { document?: Doc; note?: string };
    setDoc(saved.document ?? doc);
    setNote(saved.note || "Saved.");
  }

  return (
    <section className="chat agent-view" aria-label="Agent">
      <header className="chat-head">
        <button type="button" className="chat-back" onClick={onBack}>
          ← Wire
        </button>
        <h2>{agentId}</h2>
        {presence ? <span className="chat-state">{presence}</span> : null}
        <button type="button" className="agent-save" disabled={!doc || saving} onClick={() => void save()}>
          {saving ? "Saving" : "Save"}
        </button>
      </header>
      <div className="chat-scroll">
        <div className="chat-thread agent-form">
          {!doc && !error ? <p className="empty">Loading agent</p> : null}
          {error ? <p className="msg-error">{error}</p> : null}
          {note ? <p className="composer-note">{note}</p> : null}
          {doc
            ? fields.map((field) => (
                <FieldEditor
                  key={field.name}
                  field={field}
                  value={doc[field.name]}
                  onChange={(value) => setDoc({ ...doc, [field.name]: value })}
                />
              ))
            : null}
        </div>
      </div>
    </section>
  );
}

function FieldEditor({
  field,
  value,
  onChange,
}: {
  field: FormField;
  value: unknown;
  onChange: (value: unknown) => void;
}) {
  if (field.format === "markdown" || field.name === "instructions") {
    return <MarkdownField field={field} value={typeof value === "string" ? value : ""} onChange={onChange} />;
  }
  return (
    <label className="agent-field">
      <span className="agent-field-label">
        {field.name}
        {field.required ? " *" : ""}
      </span>
      {field.description ? <span className="agent-field-help">{field.description}</span> : null}
      <FieldControl field={field} value={value} onChange={onChange} />
    </label>
  );
}

function MarkdownField({
  field,
  value,
  onChange,
}: {
  field: FormField;
  value: string;
  onChange: (value: unknown) => void;
}) {
  const [editing, setEditing] = useState(false);
  return (
    <div className="agent-field">
      <div className="agent-field-label">
        <span>
          {field.name}
          {field.required ? " *" : ""}
        </span>
        <button type="button" className="tool-copy" onClick={() => setEditing((open) => !open)}>
          {editing ? "Preview" : "Edit"}
        </button>
      </div>
      {field.description ? <span className="agent-field-help">{field.description}</span> : null}
      {editing ? (
        <textarea
          className="agent-markdown"
          value={value}
          rows={Math.min(24, Math.max(8, value.split("\n").length))}
          onChange={(event) => onChange(event.target.value)}
        />
      ) : (
        <div className="agent-markdown-preview">
          {value ? <Markdown content={value} /> : <p className="agent-field-help">Empty.</p>}
        </div>
      )}
    </div>
  );
}

function FieldControl({
  field,
  value,
  onChange,
}: {
  field: FormField;
  value: unknown;
  onChange: (value: unknown) => void;
}) {
  if (field.enum) {
    const current = value == null ? "" : String(value);
    return <AgentSelect value={current} options={field.enum} onChange={onChange} />;
  }
  if (field.type === "integer") {
    const current = typeof value === "number" ? String(value) : "";
    return (
      <input
        type="number"
        min={1}
        value={current}
        placeholder="default"
        onChange={(event) => onChange(event.target.value === "" ? undefined : Number(event.target.value))}
      />
    );
  }
  if (field.type === "boolean") {
    return <input type="checkbox" checked={Boolean(value)} onChange={(event) => onChange(event.target.checked)} />;
  }
  if (field.type === "array") {
    const items = Array.isArray(value) ? value.map((item) => String(item)) : [];
    return (
      <div className="agent-list">
        {items.map((item, index) => (
          <div className="agent-list-row" key={index}>
            <input
              value={item}
              onChange={(event) => {
                const next = items.slice();
                next[index] = event.target.value;
                onChange(next);
              }}
            />
            <button
              type="button"
              className="tool-copy"
              onClick={() => onChange(items.filter((_, itemIndex) => itemIndex !== index))}
            >
              Remove
            </button>
          </div>
        ))}
        <button type="button" className="tool-copy" onClick={() => onChange([...items, ""])}>
          Add
        </button>
      </div>
    );
  }
  if (field.type === "map") {
    const entries = mapEntries(value);
    return (
      <div className="agent-list">
        {entries.map(([key, item], index) => (
          <div className="agent-list-row" key={index}>
            <input
              value={key}
              placeholder="name"
              onChange={(event) => {
                const next = entries.slice();
                next[index] = [event.target.value, item];
                onChange(Object.fromEntries(next));
              }}
            />
            <input
              value={item}
              placeholder="value"
              onChange={(event) => {
                const next = entries.slice();
                next[index] = [key, event.target.value];
                onChange(Object.fromEntries(next));
              }}
            />
            <button
              type="button"
              className="tool-copy"
              onClick={() => onChange(Object.fromEntries(entries.filter((_, entryIndex) => entryIndex !== index)))}
            >
              Remove
            </button>
          </div>
        ))}
        <button type="button" className="tool-copy" onClick={() => onChange(Object.fromEntries([...entries, ["", ""]]))}>
          Add
        </button>
      </div>
    );
  }
  if (field.type === "object") {
    const object = value && typeof value === "object" && !Array.isArray(value) ? (value as Doc) : {};
    return (
      <div className="agent-object">
        {(field.fields ?? []).map((child) => (
          <FieldEditor
            key={child.name}
            field={child}
            value={object[child.name]}
            onChange={(next) => onChange({ ...object, [child.name]: next })}
          />
        ))}
      </div>
    );
  }
  return (
    <input
      value={value == null ? "" : String(value)}
      onChange={(event) => onChange(event.target.value === "" ? undefined : event.target.value)}
    />
  );
}

function AgentSelect({
  value,
  options,
  onChange,
}: {
  value: string;
  options: string[];
  onChange: (value: unknown) => void;
}) {
  const [open, setOpen] = useState(false);
  const [dropUp, setDropUp] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  useLayoutEffect(() => {
    const node = root.current;
    if (!open || !node) return;
    const trigger = node.getBoundingClientRect();
    const scroller = node.closest(".chat-scroll");
    const limit = scroller ? scroller.getBoundingClientRect().bottom : window.innerHeight;
    const menuHeight = options.length * 34 + 16;
    setDropUp(limit - trigger.bottom < menuHeight + 12);
  }, [open, options.length]);
  const label = value || "default";
  return (
    <div className={`agent-select ${open ? "open" : ""} ${dropUp ? "drop-up" : ""}`} ref={root}>
      <button type="button" className="agent-select-trigger" aria-expanded={open} onClick={() => setOpen((prev) => !prev)}>
        <span>{label}</span>
        <i aria-hidden="true" />
      </button>
      {open ? (
        <ul className="agent-select-menu" role="listbox">
          {options.map((option) => {
            const current = option === value;
            return (
              <li key={option || "default"}>
                <button
                  type="button"
                  role="option"
                  aria-selected={current}
                  className={current ? "current" : ""}
                  onClick={() => {
                    onChange(option);
                    setOpen(false);
                  }}
                >
                  {option || "default"}
                </button>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}

function mapEntries(value: unknown): [string, string][] {
  if (!value || typeof value !== "object" || Array.isArray(value)) return [];
  return Object.entries(value as Record<string, unknown>).map(([key, item]) => [key, item == null ? "" : String(item)]);
}
