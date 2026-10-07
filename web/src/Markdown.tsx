import { useMemo } from "react";
import { marked } from "marked";

export interface MarkdownProps {
  content: string;
  className?: string;
}

marked.use({
  breaks: true,
  gfm: true,
});

export function Markdown({ content, className = "markdown" }: MarkdownProps) {
  const html = useMemo(() => {
    if (!content) return "";
    return marked.parse(content, { async: false }) as string;
  }, [content]);

  return (
    <div
      className={className}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
}
