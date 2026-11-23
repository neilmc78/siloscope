"use strict";

// How long ago something happened, in the two largest units that apply:
// "37 min ago", "5 hours, 12 min ago", "1 day, 23 hours ago",
// "3 weeks, 2 days ago". The second unit is the next one down and is left
// out when it is zero ("1 day ago"). A month is 30 days and a year twelve of
// them, so 11 months rolls straight into "1 year" rather than "12 months".
const TimeAgo = (() => {
  const units = [
    ["year", 360 * 86400],
    ["month", 30 * 86400],
    ["week", 7 * 86400],
    ["day", 86400],
    ["hour", 3600],
    ["min", 60],
  ];

  function count(value, name) {
    // "min" reads the same for one or many: "1 min", "12 min".
    return `${value} ${name}${value === 1 || name === "min" ? "" : "s"}`;
  }

  function format(ms, now = Date.now()) {
    const seconds = Math.floor((now - ms) / 1000);
    if (!Number.isFinite(seconds) || seconds < 60) return "Just now";
    const index = units.findIndex(([, size]) => seconds >= size);
    const [name, size] = units[index];
    const parts = [count(Math.floor(seconds / size), name)];
    const next = units[index + 1];
    if (next) {
      const rest = Math.floor((seconds % size) / next[1]);
      if (rest > 0) parts.push(count(rest, next[0]));
    }
    return `${parts.join(", ")} ago`;
  }

  return { format };
})();

if (typeof module !== "undefined" && module.exports) module.exports = TimeAgo;
