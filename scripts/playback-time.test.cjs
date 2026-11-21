// Unit tests for the Now Playing end-time estimate: node --test scripts/playback-time.test.cjs
const test = require("node:test");
const assert = require("node:assert/strict");
const PlaybackTime = require("../static/playback-time.js");

const now = Date.UTC(2026, 10, 21, 17, 0, 0);

test("ends after the time left, from now", () => {
  const end = PlaybackTime.endTime({ file_duration: 3000, position_seconds: 1800 }, now);
  assert.equal(end.getTime(), now + 1200 * 1000);
});

test("accepts numbers sent as strings", () => {
  assert.equal(PlaybackTime.remainingSeconds({ file_duration: "100", position_seconds: "40" }), 60);
});

test("a position past the end means it ends now, not in the past", () => {
  assert.equal(PlaybackTime.endTime({ file_duration: 100, position_seconds: 130 }, now).getTime(), now);
});

test("no end time when paused or when the numbers are missing or wrong", () => {
  const cases = [
    null,
    { file_duration: 3000, position_seconds: 100, is_paused: true },
    { position_seconds: 100 },
    { file_duration: 3000 },
    { file_duration: 0, position_seconds: 0 },
    { file_duration: 3000, position_seconds: -5 },
    { file_duration: "soon", position_seconds: 10 },
  ];
  for (const session of cases) assert.equal(PlaybackTime.endTime(session, now), null, JSON.stringify(session));
  assert.equal(PlaybackTime.endsLabel({ file_duration: 3000, position_seconds: 100, is_paused: true }, now), "");
});

test("the clock follows the device's 12- or 24-hour style", () => {
  const evening = new Date(2026, 10, 21, 18, 22);
  const morning = new Date(2026, 10, 21, 9, 5);
  assert.match(PlaybackTime.formatClock(evening, "en-US"), /^6:22\s?PM$/);
  assert.equal(PlaybackTime.formatClock(evening, "en-GB"), "18:22");
  assert.equal(PlaybackTime.formatClock(morning, "en-GB"), "09:05");
});

test("label reads Ends and the time", () => {
  const session = { file_duration: 3000, position_seconds: 1800 };
  assert.equal(PlaybackTime.endsLabel(session, now, "en-GB"), `Ends ${PlaybackTime.formatClock(new Date(now + 1200 * 1000), "en-GB")}`);
});
