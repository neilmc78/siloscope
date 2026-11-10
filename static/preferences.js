"use strict";

// A session that ends (expired, signed out elsewhere, or no longer an admin)
// makes every API call answer 401; send the viewer back to the sign-in page.
if (typeof window !== "undefined" && window.fetch && location.pathname !== "/login") {
  const nativeFetch = window.fetch.bind(window);
  window.fetch = async (...args) => {
    const response = await nativeFetch(...args);
    const target = String(args[0]?.url ?? args[0]);
    if (response.status === 401 && target.startsWith("/api/") && !target.startsWith("/api/auth/")) {
      location.assign(`/login?next=${encodeURIComponent(location.pathname + location.search)}`);
    }
    return response;
  };
}

const MonitorPreferences = (() => {
  const storageKey = "silo-monitor.ui.v1";
  const systemOrder = ["disk", "cpu", "network", "ifd", "memory"];
  const navOrder = ["system", "playing", "stats", "library", "infrastructure", "settings"];
  // [key, label, description, preview: background, surface, accent, text]
  const themes = [
    ["rose", "Rose", "The original dark theme", ["#100c0e", "#231a1e", "#ee8b9d", "#f5f0f2"]],
    ["midnight", "Midnight", "Deep navy, cool blue", ["#0b0f17", "#151c28", "#6cb6ff", "#eef2f8"]],
    ["light", "Light", "Bright surfaces for daytime", ["#f6f3f4", "#ffffff", "#c2445e", "#22171b"]],
    ["contrast", "High contrast", "Black, white and bold edges", ["#000000", "#000000", "#ffd600", "#ffffff"]],
    ["console", "Console", "Green phosphor terminal", ["#050805", "#0a110a", "#33ff66", "#b6ffb6"]],
    ["matrix", "Matrix", "Falling code in the background", ["#000000", "#03200a", "#00ff41", "#c8ffd4"]],
    ["synthwave", "Synthwave", "Neon pink and cyan", ["#14081f", "#20102f", "#ff4fd8", "#2de2e6"]],
  ];
  const accents = [
    ["#ee8b9d", "Rose"], ["#ff6b6b", "Red"], ["#ff9f43", "Orange"], ["#f3bd4f", "Amber"], ["#a3e635", "Lime"],
    ["#43d3a2", "Green"], ["#2dd4bf", "Teal"], ["#22d3ee", "Cyan"], ["#60a5fa", "Blue"], ["#a78bfa", "Violet"], ["#f472b6", "Pink"],
  ];
  const groups = {
    system: {
      label: "System",
      options: [
        ["heading", "Section heading", ".resource-section > .section-heading > div"],
        ["status", "Connection light", "#connection-status"],
        ["build", "Server build", "#server-build"],
        ["disk", "Disk usage", ".disk-panel"],
        ["disk.heading", "Heading", ".disk-heading", "disk"],
        ["disk.values", "Usage values", ".disk-summary", "disk"],
        ["disk.bars", "Usage bars", ".disk-track", "disk"],
        ["disk.media", "Media disks", ".disk-row.media-disk", "disk"],
        ["disk.libraries", "Library sizes", ".disk-libraries", "disk.media"],
        ["cpu", "CPU", ".cpu-panel"],
        ["cpu.heading", "Heading", ".cpu-panel .metric-section-heading", "cpu"],
        ["cpu.value", "Current usage", ".cpu-panel .metric-header > div", "cpu"],
        ["cpu.detail", "Cores and load", "#cpu-detail", "cpu"],
        ["cpu.chart", "Graph", ".cpu-panel .chart-wrap", "cpu"],
        ["cpu.scale", "Graph scale", ".cpu-panel .chart-scale", "cpu.chart"],
        ["cpu.time", "Time labels", ".cpu-panel .chart-footer", "cpu.chart"],
        ["cpu.processes", "Top CPU processes", ".cpu-processes", "cpu"],
        ["network", "Bandwidth", ".network-panel"],
        ["network.heading", "Heading", ".network-panel .metric-section-heading", "network"],
        ["network.download", "Download value", ".network-download", "network"],
        ["network.upload", "Upload value", ".network-upload", "network"],
        ["network.detail", "Bandwidth detail", "#network-detail", "network"],
        ["network.chart", "Graph", ".network-panel .chart-wrap", "network"],
        ["network.scale", "Graph scale", ".network-panel .chart-scale", "network.chart"],
        ["network.time", "Time labels", ".network-panel .chart-footer", "network.chart"],
        ["ifd", "InfiniDysk", ".ifd-panel"],
        ["ifd.heading", "Heading", ".ifd-heading", "ifd"],
        ["ifd.status", "Connection status", "#ifd-status", "ifd"],
        ["ifd.fetched", "Fetched throughput", ".ifd-fetched", "ifd"],
        ["ifd.served", "Served throughput", ".ifd-served", "ifd"],
        ["ifd.connections", "Connections in use", ".ifd-connections", "ifd"],
        ["ifd.chart", "Graph", ".ifd-panel .chart-wrap", "ifd"],
        ["ifd.scale", "Graph scale", ".ifd-panel .chart-scale", "ifd.chart"],
        ["ifd.time", "Time labels", ".ifd-panel .chart-footer", "ifd.chart"],
        ["ifd.files", "Active files", ".ifd-files", "ifd"],
        ["ifd.speed", "File fetching speed", ".ifd-file-speed", "ifd.files"],
        ["ifd.bytes", "File bytes fetched", ".ifd-file-bytes", "ifd.files"],
        ["memory", "Memory", ".ram-panel"],
        ["memory.value", "Current usage", ".ram-panel .metric-header > div", "memory"],
        ["memory.detail", "Capacity", "#ram-detail", "memory"],
        ["memory.chart", "Graph", ".ram-panel .chart-wrap", "memory"],
        ["memory.scale", "Graph scale", ".ram-panel .chart-scale", "memory.chart"],
        ["memory.time", "Time labels", ".ram-panel .chart-footer", "memory.chart"],
      ],
    },
    playing: {
      label: "Playing",
      options: [
        ["heading", "Section heading", ".sessions-section .section-heading > div"],
        ["count", "Stream count", "#session-count"],
        ["badge", "Tab badge", "#playing-badge"],
        ["cards", "Session cards", "#sessions"],
        ["posters", "Posters", ".session-card .poster", "cards"],
        ["title", "Title", ".session-title", "cards"],
        ["subtitle", "Episode and media details", ".session-subtitle", "cards"],
        ["source", "Playback source", ".session-source", "cards"],
        ["resolution", "Video resolution", ".session-resolution", "cards"],
        ["tonemap", "Tone mapping", ".session-tonemap", "cards"],
        ["audio", "Audio codec", ".session-audio", "cards"],
        ["bitrate", "File bitrate", ".session-bitrate", "cards"],
        ["method", "Play method", ".session-tags .tag-method, .session-tags .tag-transcode", "cards"],
        ["client", "Client", ".session-client", "cards"],
        ["node", "Playback node", ".session-node", "cards"],
        ["profile", "Profile", ".session-profile", "cards"],
        ["progress", "Playback progress", ".session-card .progress-track", "cards"],
        ["user", "User", ".session-footer .avatar, .session-user", "cards"],
        ["time", "Start time / playback state", ".session-time", "cards"],
      ],
    },
    infrastructure: {
      label: "Nodes",
      options: [
        ["heading", "Section heading", ".nodes-section .section-heading > div"],
        ["summary", "Health summary", "#node-summary"],
        ["badge", "Transcode job badge", "#nodes-badge"],
        ["cards", "Node cards", "#nodes"],
        ["name", "Name", ".node-name", "cards"],
        ["role", "Role", ".node-role", "cards"],
        ["health", "Health", ".node-status", "cards"],
        ["routes", "Routed streams", ".node-route", "cards"],
        ["jobs", "Jobs", ".node-stat-jobs", "cards"],
        ["egress", "Egress", ".node-stat-egress", "cards"],
        ["checked", "Last check", ".node-stat-checked", "cards"],
        ["accelerator", "Accelerator", ".node-stat-accelerator", "cards"],
        ["resources", "Resource summary", ".node-resources", "cards"],
      ],
    },
  };

  function normalize(raw) {
    const result = { visibility: {}, systemOrder: [...systemOrder], navOrder: [...navOrder], navHidden: [], historyFilter: "all", chartHeight: 140, chartMinutes: 5, theme: "rose", accent: "", effects: true };
    for (const [group, definition] of Object.entries(groups)) {
      for (const [key] of definition.options) {
        const fullKey = `${group}.${key}`;
        result.visibility[fullKey] = typeof raw?.visibility?.[fullKey] === "boolean" ? raw.visibility[fullKey] : true;
      }
    }
    if (Array.isArray(raw?.systemOrder)) {
      result.systemOrder = [...new Set([...raw.systemOrder.filter(key => systemOrder.includes(key)), ...systemOrder])];
    }
    if (Array.isArray(raw?.navOrder)) result.navOrder = [...new Set([...raw.navOrder.filter(key => navOrder.includes(key)), ...navOrder])];
    // Settings can never be hidden: it is the only way back to this option.
    if (Array.isArray(raw?.navHidden)) result.navHidden = [...new Set(raw.navHidden.filter(key => navOrder.includes(key) && key !== "settings"))];
    if (themes.some(([key]) => key === raw?.theme)) result.theme = raw.theme;
    if (typeof raw?.accent === "string" && /^#[0-9a-f]{6}$/i.test(raw.accent)) result.accent = raw.accent.toLowerCase();
    if (typeof raw?.effects === "boolean") result.effects = raw.effects;
    if (["all", "silo", "plex"].includes(raw?.historyFilter)) result.historyFilter = raw.historyFilter;
    if (Number.isFinite(raw?.chartHeight)) result.chartHeight = Math.min(220, Math.max(100, raw.chartHeight));
    if ([2, 3, 4, 5].includes(raw?.chartMinutes)) result.chartMinutes = raw.chartMinutes;
    return result;
  }

  function enabled(preferences, group, key) {
    const option = groups[group]?.options.find(option => option[0] === key);
    return !!option && preferences.visibility[`${group}.${key}`] !== false
      && (!option[3] || enabled(preferences, group, option[3]));
  }

  function load(storage) {
    try { return normalize(JSON.parse(storage.getItem(storageKey))); }
    catch { return normalize(null); }
  }

  function save(storage, preferences) {
    try { storage.setItem(storageKey, JSON.stringify(normalize(preferences))); return true; }
    catch { return false; }
  }

  // applyTheme sets the theme, accent and effects on <html>; CSS does the rest.
  function applyTheme(preferences) {
    const root = document.documentElement;
    root.dataset.theme = preferences.theme;
    root.dataset.effects = preferences.effects ? "on" : "off";
    for (const name of ["--primary", "--cpu", "--primary-soft"]) root.style.removeProperty(name);
    if (preferences.accent) {
      root.style.setProperty("--primary", preferences.accent);
      root.style.setProperty("--cpu", preferences.accent);
      root.style.setProperty("--primary-soft", `color-mix(in srgb, ${preferences.accent} 18%, var(--surface))`);
    }
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = color("--background") || "#171113";
    window.dispatchEvent(new CustomEvent("monitor-theme-change", { detail: { theme: preferences.theme, effects: preferences.effects } }));
  }

  // color reads a theme token, for SVG attributes that cannot take var().
  function color(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  // Apply the stored theme as soon as this script runs, before the app draws.
  if (typeof document !== "undefined") {
    try { applyTheme(load(window.localStorage)); } catch { applyTheme(normalize(null)); }
  }

  return { storageKey, systemOrder, navOrder, groups, themes, accents, normalize, enabled, load, save, applyTheme, color };
})();

if (typeof module !== "undefined" && module.exports) module.exports = MonitorPreferences;