"use strict";

(() => {
  const section = document.querySelector('[data-view="settings"]');
  const banner = document.getElementById("settings-restart-banner");
  const title = document.getElementById("settings-restart-title");
  const message = document.getElementById("settings-restart-message");
  const button = document.getElementById("settings-restart-now");
  const buttonLabel = document.getElementById("settings-restart-button-label");
  const cancel = document.getElementById("settings-restart-cancel");
  // The always-available Restart Server button and its on-page confirmation.
  const serverButton = document.getElementById("settings-restart-server");
  const strip = document.getElementById("settings-restart-confirm");
  const stripCancel = document.getElementById("settings-restart-confirm-cancel");
  const stripGo = document.getElementById("settings-restart-confirm-go");
  let stripTimer;
  let pending = false;
  // Restart asks for a second tap on the page itself: browser pop-ups such as
  // confirm() are blocked in embedded web views (nzb360 and similar).
  let confirming = false;
  let confirmTimer;
  let preview = false;
  let version = 0;
  let inFlight = false;
  let timer;
  let restarting = false;
  let startedAt = "";
  let restartStartedAt = "";
  let restartBegan = 0;
  let failure = "";
  const visible = () => !section.hidden && !document.hidden;

  function render(unavailable = false) {
    banner.hidden = !pending && !restarting && !failure;
    serverButton.disabled = restarting;
    button.disabled = restarting;
    buttonLabel.textContent = restarting ? "Restarting..." : confirming ? "Confirm restart" : "Restart now";
    cancel.hidden = !confirming || restarting;
    title.textContent = preview ? "Restart required (preview)" : "Silo restart required";
    message.textContent = unavailable ? "Restart status is temporarily unavailable. The last known state still requires a restart." : preview ? "Preview settings are pending a restart. Live Silo is unchanged." : "Some saved settings will take effect after Silo is restarted.";
    if (restarting) {
      title.textContent = preview ? "Restarting preview" : "Waiting for Silo to restart";
      message.textContent = "Waiting for the server to come back online.";
    }
    if (confirming && !restarting) {
      message.textContent = preview ? "Simulate a restart in the local preview? Live Silo will not restart." : "Restart Silo now? Active playback may be interrupted while the server restarts.";
    }
    if (failure) message.textContent = failure;
  }

  function stopConfirming() {
    clearTimeout(confirmTimer);
    confirming = false;
  }

  async function refresh() {
    clearTimeout(timer);
    if (!visible() || inFlight) return;
    inFlight = true;
    const requestedVersion = version;
    try {
      const response = await fetch("/api/restart-status", { cache: "no-store", signal: AbortSignal.timeout(10000) });
      const result = await response.json();
      if (!response.ok || typeof result.restart_required !== "boolean") throw new Error("Restart status unavailable");
      if (requestedVersion !== version) return;
      if (restarting && restartStartedAt && result.started_at !== restartStartedAt) {
        restarting = false;
        failure = "";
      }
      if (!restarting && result.restart_requested && !failure) {
        restarting = true;
        restartStartedAt = result.started_at;
        restartBegan = Date.now();
      }
      startedAt = result.started_at;
      if (restarting && !restartStartedAt) restartStartedAt = startedAt;
      pending = result.restart_required;
      preview = result.preview === true;
      render();
    } catch {
      if (requestedVersion === version) render(true);
    } finally {
      inFlight = false;
      if (restarting && Date.now() - restartBegan > 120000) {
        restarting = false;
        pending = true;
        failure = "Restart completion could not be confirmed. Check Silo before retrying.";
        render();
      }
      if (visible()) timer = setTimeout(refresh, restarting ? 3000 : 15000);
    }
  }

  button.addEventListener("click", async () => {
    if (restarting) return;
    if (window.TranscoderSettings?.canRestart() === false) {
      failure = "Wait for settings to finish saving, or correct invalid values, before restarting.";
      render();
      return;
    }
    if (!confirming) {
      confirming = true;
      failure = "";
      clearTimeout(confirmTimer);
      confirmTimer = setTimeout(() => { confirming = false; render(); }, 8000);
      render();
      return;
    }
    stopConfirming();
    await requestRestart();
  });

  // requestRestart asks Silo to restart; the banner then follows its progress.
  async function requestRestart() {
    if (restarting) return;
    if (window.TranscoderSettings?.canRestart() === false) {
      failure = "Wait for settings to finish saving, or correct invalid values, before restarting.";
      render();
      return;
    }
    version++;
    restarting = true;
    restartStartedAt = startedAt;
    restartBegan = Date.now();
    failure = "";
    clearTimeout(timer);
    render();
    try {
      const response = await fetch("/api/restart", { method: "POST", headers: { "Content-Type": "application/json", "X-Siloscope-Settings": "1" }, body: "{}", signal: AbortSignal.timeout(10000) });
      if ([400, 403, 503].includes(response.status)) {
        restarting = false;
        failure = "Silo rejected the restart request or restart is unavailable.";
      } else if (response.status !== 202) {
        failure = "Restart request could not be confirmed. Checking server status...";
      }
    } catch {
      failure = "Restart request could not be confirmed. Checking server status...";
    }
    render();
    refresh();
  }

  function closeStrip() {
    clearTimeout(stripTimer);
    strip.hidden = true;
    serverButton.setAttribute("aria-expanded", "false");
  }

  serverButton.addEventListener("click", () => {
    if (restarting) return;
    if (!strip.hidden) {
      closeStrip();
      return;
    }
    strip.hidden = false;
    serverButton.setAttribute("aria-expanded", "true");
    clearTimeout(stripTimer);
    stripTimer = setTimeout(closeStrip, 15000);
    stripGo.focus();
  });

  stripCancel.addEventListener("click", () => {
    closeStrip();
    serverButton.focus();
  });

  stripGo.addEventListener("click", async () => {
    closeStrip();
    await requestRestart();
  });

  cancel.addEventListener("click", () => {
    stopConfirming();
    render();
    button.focus();
  });

  window.addEventListener("monitor-restart-required", event => {
    version++;
    pending = true;
    preview = event.detail?.preview === true;
    render();
  });
  window.addEventListener("monitor-view-change", refresh);
  document.addEventListener("visibilitychange", refresh);
  refresh();
})();