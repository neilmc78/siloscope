"use strict";

// Estimates when an active playback session will finish, in the viewer's
// local time zone. The calculation runs in the browser, so the time shown
// follows the device viewing the dashboard rather than the container or server.
const PlaybackTime = (() => {
  function remainingSeconds(session) {
    const duration = Number(session?.file_duration);
    const position = Number(session?.position_seconds);
    if (!Number.isFinite(duration) || !Number.isFinite(position) || duration <= 0 || position < 0) return null;
    return Math.max(0, duration - position);
  }

  // Paused sessions have no meaningful finish time, so none is shown.
  function endTime(session, now = Date.now()) {
    if (!session || session.is_paused) return null;
    const remaining = remainingSeconds(session);
    if (remaining === null) return null;
    return new Date(now + remaining * 1000);
  }

  // The viewer's own clock style: "6:22 PM" where the device uses a 12-hour
  // clock, "18:22" where it uses 24-hour time. locales is for tests.
  function formatClock(date, locales = undefined) {
    return date.toLocaleTimeString(locales, { timeStyle: "short" });
  }

  function endsLabel(session, now = Date.now(), locales = undefined) {
    const end = endTime(session, now);
    return end ? `Ends ${formatClock(end, locales)}` : "";
  }

  return { remainingSeconds, endTime, formatClock, endsLabel };
})();

if (typeof module !== "undefined" && module.exports) module.exports = PlaybackTime;
