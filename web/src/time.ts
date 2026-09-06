/** Compact duration vocabulary shared with the session rail. */
export function formatDuration(ms: number): string {
  const age = Math.max(0, ms);
  const minute = 60_000, hour = 60 * minute, day = 24 * hour;
  if (age < minute) return "<1m";
  if (age < hour) return `${Math.floor(age / minute)}m`;
  if (age < day) return `${Math.floor(age / hour)}h`;
  return `${Math.floor(age / day)}d`;
}

/** Near times use the rail's compact vocabulary; distant times name a local date. */
export function formatTime(value: string, now: Date = new Date()): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "Unknown time";
  const delta = date.getTime() - now.getTime();
  if (Math.abs(delta) < 24 * 60 * 60_000) {
    const duration = formatDuration(Math.abs(delta));
    return delta >= 0 ? `in ${duration}` : `${duration} ago`;
  }
  return date.toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}
