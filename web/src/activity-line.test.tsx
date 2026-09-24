// @vitest-environment happy-dom
import { render, type VNode } from "preact";
import { afterEach, describe, expect, it } from "vitest";
import { ActivityLine } from "./activity-line";
import type { ChatLine } from "./model";

const assistant: ChatLine = {
  kind: "assistant",
  key: "assistant",
  time: "12:00",
  text: "look at the tree\n\nthe bench is clean",
};

const tool: ChatLine = {
  kind: "tool",
  key: "tool",
  time: "12:01",
  toolCall: true,
  text: 'bash\n{\n  "command": "pwd"\n}',
};

function mount(node: VNode) {
  const host = document.createElement("div");
  document.body.append(host);
  render(node, host);
  return host;
}

function row(host: ParentNode, index = 0): HTMLButtonElement {
  const button = host.querySelectorAll("button")[index];
  if (!(button instanceof HTMLButtonElement)) throw new Error("missing activity row");
  return button;
}

async function activate(button: HTMLButtonElement) {
  button.click();
  await Promise.resolve();
}

afterEach(() => {
  document.body.replaceChildren();
});

describe("activity row toggle", () => {
  it("uses the row itself as the button and toggles the full text", async () => {
    const host = mount(<ActivityLine line={assistant} />);
    const collapsed = row(host);
    expect(collapsed.getAttribute("type")).toBe("button");
    expect(collapsed.querySelector("button")).toBeNull();
    expect(collapsed.getAttribute("aria-expanded")).toBe("false");
    expect(collapsed.querySelector(".preview")?.textContent).toBe("look at the tree the bench is clean");
    expect(collapsed.querySelector(".full")).toBeNull();

    await activate(collapsed);
    const expanded = row(host);
    expect(expanded.getAttribute("aria-expanded")).toBe("true");
    expect(expanded.querySelector(".full")?.textContent).toBe(assistant.text);
    expect(expanded.querySelector(".preview")).toBeNull();

    await activate(expanded);
    const closed = row(host);
    expect(closed.getAttribute("aria-expanded")).toBe("false");
    expect(closed.querySelector(".preview")?.textContent).toBe("look at the tree the bench is clean");
    expect(closed.querySelector(".full")).toBeNull();
  });

  it("expands one row without opening another", async () => {
    const host = mount(
      <div>
        <ActivityLine line={assistant} />
        <ActivityLine line={tool} />
      </div>,
    );
    await activate(row(host, 0));
    expect(row(host, 0).getAttribute("aria-expanded")).toBe("true");
    expect(row(host, 0).querySelector(".full")?.textContent).toBe(assistant.text);
    expect(row(host, 1).getAttribute("aria-expanded")).toBe("false");
    expect(row(host, 1).querySelector(".preview")?.textContent).toBe('bash {"command":"pwd"}');
    expect(row(host, 1).querySelector(".full")).toBeNull();
  });
});
