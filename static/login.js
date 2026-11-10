"use strict";

// Sign-in page: the password step, then the two-step code step when Silo
// asks for one. On success, return to the page the visitor was trying to open.
(() => {
  const $ = id => document.getElementById(id);
  const loginForm = $("login-form");
  const secondForm = $("second-form");
  let challenge = "";
  let methods = [];
  let useRecovery = false;

  function destination() {
    const next = new URLSearchParams(location.search).get("next") || "/";
    // Only same-site paths: never an absolute or protocol-relative URL.
    return next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/\\") ? next : "/";
  }

  function showError(element, message) {
    element.textContent = message;
    element.hidden = !message;
  }

  async function post(path, body) {
    const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), credentials: "same-origin" });
    let payload = {};
    try { payload = await response.json(); } catch {}
    if (!response.ok) throw new Error(payload.error || "Sign-in failed. Try again.");
    return payload;
  }

  function busy(button, on, label) {
    button.disabled = on;
    button.textContent = on ? "Signing in…" : label;
  }

  function showSecondStep() {
    loginForm.hidden = true;
    secondForm.hidden = false;
    useRecovery = !methods.includes("totp") && methods.includes("recovery");
    updateSecondStep();
  }

  function updateSecondStep() {
    $("second-label").textContent = useRecovery ? "Recovery code" : "Authenticator code";
    $("second-hint").textContent = useRecovery
      ? "Enter one of the recovery codes you saved when you set up two-step sign-in."
      : "Enter the six-digit code from your authenticator app.";
    const input = $("second-code");
    input.value = "";
    input.inputMode = useRecovery ? "text" : "numeric";
    input.autocomplete = useRecovery ? "off" : "one-time-code";
    const switcher = $("second-switch");
    switcher.hidden = !(methods.includes("totp") && methods.includes("recovery"));
    switcher.textContent = useRecovery ? "Use your authenticator app instead" : "Use a recovery code instead";
    showError($("second-error"), "");
    input.focus();
  }

  $("login-reveal").addEventListener("click", event => {
    const input = $("login-password");
    const reveal = input.type === "password";
    input.type = reveal ? "text" : "password";
    event.currentTarget.textContent = reveal ? "Hide" : "Show";
    event.currentTarget.setAttribute("aria-pressed", String(reveal));
    event.currentTarget.setAttribute("aria-label", reveal ? "Hide password" : "Show password");
  });

  loginForm.addEventListener("submit", async event => {
    event.preventDefault();
    const username = $("login-username").value.trim();
    const password = $("login-password").value;
    if (!username || !password) {
      showError($("login-error"), "Enter your username and password.");
      return;
    }
    const button = $("login-submit");
    busy(button, true, "Sign in");
    showError($("login-error"), "");
    try {
      const result = await post("/api/auth/login", { username, password });
      if (result.second_factor) {
        challenge = result.challenge;
        methods = result.methods || [];
        if (!methods.length) throw new Error("This account needs a passkey to sign in, which Siloscope doesn't support. Add an authenticator app in Silo.");
        showSecondStep();
      } else {
        location.replace(destination());
      }
    } catch (error) {
      showError($("login-error"), error.message);
      $("login-password").select();
    } finally {
      busy(button, false, "Sign in");
    }
  });

  secondForm.addEventListener("submit", async event => {
    event.preventDefault();
    const code = $("second-code").value.trim();
    if (!code) {
      showError($("second-error"), "Enter a code.");
      return;
    }
    const button = $("second-submit");
    busy(button, true, "Verify");
    try {
      await post("/api/auth/second-factor", useRecovery ? { challenge, recovery_code: code } : { challenge, code });
      location.replace(destination());
    } catch (error) {
      showError($("second-error"), error.message);
      $("second-code").select();
    } finally {
      busy(button, false, "Verify");
    }
  });

  $("second-switch").addEventListener("click", () => {
    useRecovery = !useRecovery;
    updateSecondStep();
  });

  $("second-restart").addEventListener("click", () => {
    challenge = "";
    secondForm.hidden = true;
    loginForm.hidden = false;
    $("login-password").value = "";
    showError($("login-error"), "");
    $("login-password").focus();
  });
})();
