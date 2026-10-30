/** href for an agent-supplied link: only http and https URLs, else null. */
export function safeHttpUrl(raw: string | undefined): string | null {
  if (!raw) return null;
  try {
    const u = new URL(raw);
    return (u.protocol === "http:" || u.protocol === "https:") && u.host ? u.href : null;
  } catch {
    return null;
  }
}
