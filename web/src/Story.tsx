import { categoryById, type Edition } from "./editions";

export function Story({ edition }: { edition: Edition }) {
  const category = categoryById(edition.category);
  return (
    <article id={`lead-${edition.id}`} className={`story tone-${category.tone}`}>
      <div className="kicker">
        <b>{category.label}</b>
        <time>{edition.time}</time>
      </div>
      <h2>{edition.headline}</h2>
      <p className="lede">{edition.lede}</p>
      {edition.body ? (
        <details className="story-body">
          <summary>Full account</summary>
          <pre>{edition.body}</pre>
        </details>
      ) : null}
      <p className="byline">
        {edition.byline.map((part) => (
          <span key={part}>{part}</span>
        ))}
      </p>
    </article>
  );
}
