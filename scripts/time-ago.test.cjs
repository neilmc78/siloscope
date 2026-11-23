// Unit tests for History and Stats "ago" labels: node --test scripts/time-ago.test.cjs
const test = require("node:test");
const assert = require("node:assert/strict");
const TimeAgo = require("../static/time-ago.js");

const now = Date.UTC(2026, 10, 23, 12, 0, 0);
const ago = seconds => TimeAgo.format(now - seconds * 1000, now);
const m = 60, h = 3600, d = 86400;

test("two largest units, rolling over at each boundary", () => {
  const cases = [
    [59, "Just now"],
    [60, "1 min ago"],
    [37 * m, "37 min ago"],
    [59 * m, "59 min ago"],
    [h, "1 hour ago"],
    [5 * h + 12 * m, "5 hours, 12 min ago"],
    [23 * h + 59 * m, "23 hours, 59 min ago"],
    [24 * h, "1 day ago"],
    [47 * h, "1 day, 23 hours ago"],
    [102 * h, "4 days, 6 hours ago"],
    [6 * d, "6 days ago"],
    [7 * d, "1 week ago"],
    [23 * d, "3 weeks, 2 days ago"],
    [29 * d, "4 weeks, 1 day ago"],
    [30 * d, "1 month ago"],
    [70 * d, "2 months, 1 week ago"],
    [359 * d, "11 months, 4 weeks ago"],
    [360 * d, "1 year ago"],
    [430 * d, "1 year, 2 months ago"],
  ];
  for (const [seconds, label] of cases) assert.equal(ago(seconds), label, `${seconds}s`);
});

test("a zero second unit is left out, not shown as 0", () => {
  assert.equal(ago(2 * d + 59), "2 days ago");
  assert.equal(ago(3 * h + 30), "3 hours ago");
});

test("a time in the future or an invalid one reads Just now", () => {
  assert.equal(ago(-120), "Just now");
  assert.equal(TimeAgo.format(NaN, now), "Just now");
});
