import { categoryById, type Edition } from "./editions";
import { Markdown } from "./Markdown";

export function Story({ edition, onRead }: { edition: Edition; onRead?: (id: string) => void }) {
  const category = categoryById(edition.category);
  return (
    <article id={`lead-${edition.id}`} className={`story tone-${category.tone}`}>
      <div className="kicker">
        <b>{category.label}</b>
        <time>{edition.time}</time>
      </div>
      <h2>{edition.headline}</h2>
      <p className="lede">{edition.lede}</p>
      {edition.body && onRead ? (
        <button type="button" className="story-more" onClick={() => onRead(edition.id)}>
          › read more
        </button>
      ) : null}
      <p className="byline">
        {edition.byline.map((part) => (
          <span key={part}>{part}</span>
        ))}
      </p>
    </article>
  );
}

export function PublicationPage({ edition, onBack }: { edition: Edition; onBack: () => void }) {
  const category = categoryById(edition.category);
  return (
    <section className={`chat publication tone-${category.tone}`} aria-label="Publication">
      <header className="chat-head">
        <button type="button" className="chat-back" onClick={onBack} title="Back to Wire" aria-label="Back to wire">
          ← Wire
        </button>
        <h2>{category.label}</h2>
        <span className="chat-meta">{edition.time}</span>
      </header>
      <div className="chat-scroll">
        <article className="chat-thread publication-body">
          <h2>{edition.headline}</h2>
          <p className="lede">{edition.lede}</p>
          {edition.body ? <Markdown content={edition.body} /> : null}
          <p className="byline">
            {edition.byline.map((part) => (
              <span key={part}>{part}</span>
            ))}
          </p>
        </article>
      </div>
    </section>
  );
}
