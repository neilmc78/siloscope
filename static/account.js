"use strict";

// Shows who is signed in on the Settings tab, with a Sign out button.
(async () => {
  const bar = document.getElementById("account-bar");
  if (!bar) return;
  try {
    const response = await fetch("/api/auth/status", { cache: "no-store" });
    const status = await response.json();
    if (!status.required || !status.signed_in) return;
    document.getElementById("account-name").textContent = status.username || "Silo admin";
    bar.hidden = false;
  } catch {
    return;
  }
  document.getElementById("account-signout").addEventListener("click", async () => {
    try { await fetch("/api/auth/logout", { method: "POST" }); } catch {}
    location.replace("/login");
  });
})();

// "Open from Silo": the secret the Siloscope plugin in Silo signs launches
// with. Confirmations stay on the page; nzb360 and other embedded browsers
// drop confirm() pop-ups.
(async () => {
  const card = document.getElementById("launch-card");
  if (!card) return;
  const $ = id => document.getElementById(id);
  const input = $("launch-secret");
  const status = $("launch-status");
  const confirmStrip = $("launch-regenerate-confirm");
  let confirmTimer = 0;

  function show(state) {
    input.value = state.secret || "";
    const last = $("launch-last");
    last.textContent = state.last_launch
      ? `Last opened from Silo ${new Date(state.last_launch).toLocaleString()}${state.last_user ? ` by ${state.last_user}` : ""}.`
      : "Not opened from Silo yet.";
    $("launch-regenerate").hidden = Boolean(state.from_env);
    if (state.from_env) last.textContent += " Set by SILO_LAUNCH_SECRET.";
  }

  try {
    const response = await fetch("/api/auth/launch", { cache: "no-store" });
    if (!response.ok) return;
    const state = await response.json();
    if (state.disabled) return;
    show(state);
    card.hidden = false;
  } catch {
    return;
  }

  $("launch-reveal").addEventListener("click", event => {
    const reveal = input.type === "password";
    input.type = reveal ? "text" : "password";
    event.currentTarget.textContent = reveal ? "Hide" : "Show";
    event.currentTarget.setAttribute("aria-pressed", String(reveal));
  });

  $("launch-copy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(input.value);
      status.textContent = "Copied. Paste it into Launch secret in Silo's Siloscope plugin.";
    } catch {
      // The clipboard API needs https; fall back to a selection the person copies.
      input.type = "text";
      $("launch-reveal").textContent = "Hide";
      input.select();
      status.textContent = "Selected. Copy it, then paste it into Launch secret in Silo's Siloscope plugin.";
    }
  });

  function closeConfirm() {
    clearTimeout(confirmTimer);
    confirmStrip.hidden = true;
    $("launch-regenerate").setAttribute("aria-expanded", "false");
  }
  $("launch-regenerate").addEventListener("click", () => {
    confirmStrip.hidden = false;
    $("launch-regenerate").setAttribute("aria-expanded", "true");
    clearTimeout(confirmTimer);
    confirmTimer = setTimeout(closeConfirm, 15000);
  });
  $("launch-regenerate-cancel").addEventListener("click", closeConfirm);
  $("launch-regenerate-go").addEventListener("click", async event => {
    const button = event.currentTarget;
    button.disabled = true;
    try {
      const response = await fetch("/api/auth/launch/regenerate", {
        method: "POST", headers: { "Content-Type": "application/json", "X-Siloscope-Settings": "1" }, body: "{}",
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(payload.error || "Could not make a new secret.");
      input.value = payload.secret;
      status.textContent = "New secret made. Paste it into Launch secret in Silo's Siloscope plugin.";
      closeConfirm();
    } catch (error) {
      status.textContent = error.message;
    } finally {
      button.disabled = false;
    }
  });
})();
