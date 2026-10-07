export type Tone = "blue" | "green" | "red" | "amber" | "violet";

export interface Category {
  id: string;
  label: string;
  tone: Tone;
}

export const categories: Category[] = [
  { id: "report", label: "Report", tone: "blue" },
  { id: "progress", label: "Progress", tone: "green" },
  { id: "breakthrough", label: "Breakthrough", tone: "amber" },
  { id: "impasse", label: "Impasse", tone: "red" },
];

export interface Edition {
  id: string;
  time: string;
  category: string;
  headline: string;
  lede: string;
  byline: string[];
}

// The feed is empty until stories are read from the publication register.
// Nothing here is sample data: the page shows what the control API returns.
export const feed: Edition[] = [];

export function categoryById(id: string) {
  const found = categories.find((item) => item.id === id);
  if (!found) throw new Error(`Unknown category ${id}`);
  return found;
}
