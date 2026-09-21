export async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text || `${response.status}`);
  }
  return JSON.parse(text) as T;
}

export async function postText(path: string, contentType: string, body: string): Promise<{ status: number; text: string }> {
  const response = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": contentType },
    body,
  });
  return { status: response.status, text: await response.text() };
}
