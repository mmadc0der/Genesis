import { describe, expect, it } from "vitest";
import { marked } from "marked";

describe("Markdown parsing", () => {
  it("renders bold, inline code, and lists", () => {
    const raw = "**bold text** and `inline code`\n\n- item 1\n- item 2";
    const html = marked.parse(raw, { async: false }) as string;
    expect(html).toContain("<strong>bold text</strong>");
    expect(html).toContain("<code>inline code</code>");
    expect(html).toContain("<li>item 1</li>");
    expect(html).toContain("<li>item 2</li>");
  });

  it("renders fenced code blocks", () => {
    const raw = "```bash\nls -la\n```";
    const html = marked.parse(raw, { async: false }) as string;
    expect(html).toContain("<pre><code");
    expect(html).toContain("ls -la");
  });

  it("renders markdown tables", () => {
    const raw = "| A | B |\n|---|---|\n| 1 | 2 |";
    const html = marked.parse(raw, { async: false }) as string;
    expect(html).toContain("<table>");
    expect(html).toContain("<th>A</th>");
    expect(html).toContain("<td>1</td>");
  });
});
