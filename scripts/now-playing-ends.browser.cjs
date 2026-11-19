// Browser check for the Now Playing "Ends HH:MM" line and card text size.
// Serve static/ (e.g. `python3 -m http.server 18198 -d static`) or a running
// Siloscope with AUTH_MODE=none, then: node scripts/now-playing-ends.browser.cjs
process.env.TZ = process.env.TZ || "UTC";
const { chromium } = require("playwright");
const assert = require("node:assert/strict");
const preferences = require("../static/preferences.js");
const baseURL = process.env.NOTIFICATION_PREVIEW_URL || "http://127.0.0.1:18198";
const shots = process.env.SCREENSHOT_DIR || "/tmp";

const now = Date.now();
const ago = minutes => new Date(now - minutes * 60_000).toISOString();
const silo = [
  { session_id: "a", user_id: 1, username: "alex.m", series_name: "Harbor Lights", media_title: "The Lighthouse Keeper", episode_name: "The Lighthouse Keeper", season_number: 2, episode_number: 4, play_method: "transcode", video_decision: "transcode", transcode_hw_accel: "vaapi", source_video_resolution: "4k", target_resolution: "1080p", source_audio_codec: "eac3", source_bitrate_kbps: 38000, client_label: "Living Room Apple TV", profile_name: "Alex", file_duration: 3000, position_seconds: 1800, started_at: ago(31) },
  { session_id: "b", user_id: 2, username: "sam_k", media_title: "The Quiet Hour", media_type: "movie", play_method: "directplay", source_video_resolution: "1080p", source_audio_codec: "aac", source_bitrate_kbps: 9800, client_label: "Firefox on Linux", profile_name: "Sam", file_duration: 6000, position_seconds: 4000, is_paused: true, started_at: ago(70) },
];
const plex = [
  { session_key: "c", username: "Priya", series_name: "Night Shift", media_title: "Graveyard Rounds", episode_name: "Graveyard Rounds", season_number: 1, episode_number: 7, play_method: "directplay", source_video_resolution: "1080p", source_audio_codec: "eac3", source_bitrate_kbps: 8400, client_label: "Bedroom Shield", file_duration: 2700, position_seconds: 900, playback_state: "playing" },
  { session_key: "d", username: "Casey", media_title: "Copper Valley", media_type: "movie", play_method: "transcode", target_resolution: "720p", source_audio_codec: "aac", source_bitrate_kbps: 6000, client_label: "Kitchen Roku", file_duration: 7200, position_seconds: 2000, playback_state: "playing" },
];
const clock = seconds => {
  const date = new Date(now + seconds * 1000);
  return `Ends ${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
};

async function cardMetrics(page) {
  return page.locator(".session-card").evaluateAll(cards => cards.map(card => {
    const box = element => element && element.getBoundingClientRect();
    const avatar = box(card.querySelector(".session-footer .avatar"));
    const when = box(card.querySelector(".session-when"));
    const time = box(card.querySelector(".session-time"));
    const ends = card.querySelector(".session-ends");
    const endsBox = box(ends);
    const footer = box(card.querySelector(".session-footer"));
    return {
      title: card.querySelector(".session-title").textContent,
      ends: ends && getComputedStyle(ends).display !== "none" ? ends.textContent : "",
      avatarMid: avatar.top + avatar.height / 2,
      whenMid: when.top + when.height / 2,
      endsBelowTime: endsBox ? endsBox.top >= time.bottom - 1 : null,
      rightAligned: endsBox ? Math.abs(endsBox.right - time.right) < 1 : null,
      insideFooter: when.right <= footer.right + 0.5,
      wrapped: when.top >= avatar.bottom - 0.5,
      userWidth: box(card.querySelector(".session-user")).width,
      titleSize: parseFloat(getComputedStyle(card.querySelector(".session-title")).fontSize),
      footerSize: parseFloat(getComputedStyle(card.querySelector(".session-footer")).fontSize),
    };
  }));
}

(async () => {
  const browser = await chromium.launch();
  try {
    for (const width of [320, 390, 1280]) {
      const page = await browser.newPage({ viewport: { width, height: width === 1280 ? 900 : 844 }, deviceScaleFactor: 2 });
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.route("**/api/**", route => {
        const url = route.request().url();
        if (url.includes("/api/plex/sessions")) return route.fulfill({ json: { enabled: true, sessions: plex } });
        if (url.includes("/api/sessions")) return route.fulfill({ json: silo });
        return route.fulfill({ json: [] });
      });
      await page.goto(baseURL);
      await page.locator('[data-tab="playing"]').click();
      await page.locator(".session-card").nth(3).waitFor();

      const base = await cardMetrics(page);
      assert.deepEqual(base.map(card => card.ends), [clock(1200), "", clock(1800), clock(5200)], "end times; paused card has none");
      for (const card of base) {
        assert.ok(!card.wrapped, `${card.title}: no wrap at default size`);
        assert.ok(Math.abs(card.avatarMid - card.whenMid) <= 1, `${card.title}: time block centred on avatar (${card.avatarMid} vs ${card.whenMid})`);
        assert.ok(card.insideFooter, `${card.title}: time block inside footer`);
        if (card.ends) {
          assert.equal(card.endsBelowTime, true, `${card.title}: ends sits under start time`);
          assert.equal(card.rightAligned, true, `${card.title}: ends right-aligned with start time`);
        }
      }
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "no horizontal scroll");
      await page.locator(".page-shell").evaluate(element => { element.scrollTop = 0; });
      await page.screenshot({ path: `${shots}/ends-${width}-100.png` });

      // Text size setting scales the card text and is saved.
      await page.locator('[data-tab="settings"]').click();
      await page.locator('[data-group="playing"]').click();
      const slider = page.getByLabel("Session card text size", { exact: true });
      await slider.press("End");
      let stored = await page.evaluate(key => JSON.parse(localStorage.getItem(key)), preferences.storageKey);
      assert.equal(stored.cardTextScale, 150);
      await page.locator('[data-tab="playing"]').click();
      const large = await cardMetrics(page);
      assert.ok(Math.abs(large[0].titleSize / base[0].titleSize - 1.5) < 0.02, "title text scales to 150%");
      if (width >= 375) assert.ok(large[0].footerSize > base[0].footerSize, "footer text grows where there is room");
      for (const card of large) {
        assert.ok(!card.wrapped && Math.abs(card.avatarMid - card.whenMid) <= 1 && card.insideFooter, `${card.title}: time block beside and centred on user at 150%`);
        assert.ok(card.userWidth >= card.footerSize * 3.9, `${card.title}: username keeps ~4em at 150% (${card.userWidth.toFixed(1)}px)`);
      }
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "no horizontal scroll at 150%");
      await page.locator(".page-shell").evaluate(element => { element.scrollTop = 0; });
      await page.screenshot({ path: `${shots}/ends-${width}-150.png` });

      await page.locator('[data-tab="settings"]').click();
      await slider.press("Home");
      await page.locator('[data-tab="playing"]').click();
      const small = await cardMetrics(page);
      assert.ok(small[0].footerSize < base[0].footerSize, "footer text shrinks at 80%");
      assert.ok(Math.abs(small[0].titleSize / base[0].titleSize - 0.8) < 0.02, "title text scales to 80%");
      await page.screenshot({ path: `${shots}/ends-${width}-80.png` });

      // The Estimated end time toggle hides the line; the start time stays.
      await page.locator('[data-tab="settings"]').click();
      await page.locator('input[data-key="ends"]').uncheck();
      stored = await page.evaluate(key => JSON.parse(localStorage.getItem(key)), preferences.storageKey);
      assert.equal(stored.visibility["playing.ends"], false);
      await page.locator('[data-tab="playing"]').click();
      const hidden = await cardMetrics(page);
      assert.deepEqual(hidden.map(card => card.ends), ["", "", "", ""]);
      for (const card of hidden) assert.ok(!card.wrapped && Math.abs(card.avatarMid - card.whenMid) <= 1, `${card.title}: start time centred when end time hidden`);
      await page.screenshot({ path: `${shots}/ends-${width}-hidden.png` });

      // Reset for the Playing tab restores both.
      await page.locator('[data-tab="settings"]').click();
      await page.locator("#reset-tab-settings").click();
      assert.ok(await page.locator('input[data-key="ends"]').isChecked());
      assert.equal(await slider.inputValue(), "100");
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log("chromium: end time placement, alignment, text size, toggle and reset passed");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
