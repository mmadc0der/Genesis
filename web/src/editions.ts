export type Tone = "blue" | "green" | "red" | "amber" | "violet";

export interface Category {
  id: string;
  label: string;
  tone: Tone;
}

// The kinds the register accepts (docs/publications.md). Each tone is the
// colour the Wire gives that kind.
export const categories: Category[] = [
  { id: "report", label: "Report", tone: "blue" },
  { id: "progress", label: "Progress", tone: "green" },
  { id: "breakthrough", label: "Breakthrough", tone: "amber" },
  { id: "impasse", label: "Impasse", tone: "red" },
];

// An Edition is one publication prepared for display.
export interface Edition {
  id: string;
  time: string;
  category: string;
  headline: string;
  lede: string;
  body?: string;
  byline: string[];
}

// A kind this build does not know is shown as itself in the report colour
// instead of failing the page.
export function categoryById(id: string): Category {
  return categories.find((item) => item.id === id) ?? { id, label: id, tone: "blue" };
}
