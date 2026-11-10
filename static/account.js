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
