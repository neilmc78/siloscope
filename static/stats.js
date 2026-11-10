"use strict";

// Stats tab: watch statistics across Silo and Plex, an overview and a page per
// person. Data comes from /api/stats and /api/stats/users; charts are plain SVG.
const MonitorStats = (() => {
  const root = document.getElementById("stats-root");
  if (!root) return null;

  // Validated with the dataviz palette checker against the card surface.
  // Series colours come from the theme (checked for colourblind separation).
  const COLORS = {};
  function readColors() {
    const color = MonitorPreferences.color;
    Object.assign(COLORS, { series: color("--series-1"), transcode: color("--series-2"), direct_play: color("--series-3"), direct_stream: color("--series-1") });
  }
  readColors();
  const METHOD_LABELS = { direct_play: "Direct play", direct_stream: "Direct stream", transcode: "Transcode" };
  const RANGES = [["7d", "Week"], ["30d", "Month"], ["365d", "Year"], ["all", "All"]];
  const SOURCES = [["all", "All"], ["silo", "Silo"], ["plex", "Plex"]];
  const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

  const state = { people: [], notice: "", view: "overview", range: "7d", source: "all", person: null, data: null, users: null, sort: "plays", query: "", loading: false, error: "" };
  let request = 0;

  function make(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = text;
    return node;
  }

  function svg(tag, attributes = {}) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
    return node;
  }

  function plural(count, word) {
    return `${Number(count).toLocaleString()} ${word}${count === 1 ? "" : "s"}`;
  }

  function duration(seconds) {
    const total = Math.round(Math.max(0, Number(seconds) || 0) / 60);
    if (total >= 60 * 24) return `${Math.floor(total / 1440)}d ${Math.floor((total % 1440) / 60)}h`;
    if (total >= 60) return `${Math.floor(total / 60)}h ${total % 60}m`;
    return `${total}m`;
  }

  function relative(ms) {
    if (!ms) return "Never";
    const seconds = Math.max(0, (Date.now() - ms) / 1000);
    if (seconds < 60) return "Just now";
    if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
    const days = Math.floor(seconds / 86400);
    return days === 1 ? "1 day ago" : `${days} days ago`;
  }

  function bucketLabel(iso, bucket, long = false) {
    const date = new Date(iso);
    if (bucket === "month") return date.toLocaleDateString(undefined, { month: "short", year: long ? "numeric" : "2-digit" });
    if (bucket === "week" && long) return `Week of ${date.toLocaleDateString(undefined, { day: "numeric", month: "short" })}`;
    return date.toLocaleDateString(undefined, long ? { weekday: "short", day: "numeric", month: "short" } : { day: "numeric", month: "short" });
  }

  function hourLabel(hour) {
    return new Date(2000, 0, 1, hour).toLocaleTimeString(undefined, { hour: "numeric" });
  }

  function niceMax(value) {
    if (value <= 4) return Math.max(1, Math.ceil(value));
    const magnitude = 10 ** Math.floor(Math.log10(value));
    for (const step of [1, 2, 2.5, 5, 10]) if (step * magnitude >= value) return step * magnitude;
    return 10 * magnitude;
  }

  // A card with a title, optional legend, chart body and a hidden data table.
  function card(title, extraClass = "") {
    const node = make("section", `stats-card ${extraClass}`.trim());
    node.append(make("h3", "stats-card-title", title));
    return node;
  }

  function dataTable(caption, headers, rows) {
    const table = make("table", "visually-hidden");
    table.append(make("caption", "", caption));
    const head = make("tr");
    for (const header of headers) head.append(make("th", "", header));
    table.append(head);
    for (const row of rows) {
      const line = make("tr");
      for (const cell of row) line.append(make("td", "", String(cell)));
      table.append(line);
    }
    return table;
  }

  function tooltipLayer(wrap) {
    const tip = make("div", "stats-tooltip");
    tip.hidden = true;
    wrap.append(tip);
    return {
      show(x, y, lines) {
        tip.replaceChildren(...lines.map((line, index) => make(index === 0 ? "strong" : "span", "", line)));
        tip.hidden = false;
        const width = wrap.clientWidth;
        const tipWidth = tip.offsetWidth;
        tip.style.left = `${Math.min(Math.max(0, x - tipWidth / 2), Math.max(0, width - tipWidth))}px`;
        tip.style.top = `${Math.max(0, y - tip.offsetHeight - 10)}px`;
      },
      hide() { tip.hidden = true; },
    };
  }

  const PAD = { top: 10, right: 8, bottom: 22, left: 30 };

  function axes(chart, width, height, maxValue, labels, unit = "") {
    const plotHeight = height - PAD.top - PAD.bottom;
    for (const fraction of [0, 0.5, 1]) {
      const y = PAD.top + plotHeight * (1 - fraction);
      chart.append(svg("line", { x1: PAD.left, x2: width - PAD.right, y1: y, y2: y, class: "stats-grid" }));
      const label = svg("text", { x: PAD.left - 6, y: y + 3, "text-anchor": "end", class: "stats-axis" });
      label.textContent = `${Math.round(maxValue * fraction * 10) / 10}${fraction ? unit : ""}`;
      chart.append(label);
    }
    for (const { x, text, anchor } of labels) {
      const label = svg("text", { x, y: height - 6, "text-anchor": anchor || "middle", class: "stats-axis" });
      label.textContent = text;
      chart.append(label);
    }
  }

  // Line chart over time: one or two series sharing one axis.
  function lineChart(series, points, bucket, format, unit = "") {
    const wrap = make("div", "stats-chart");
    const width = Math.max(260, root.clientWidth - 34);
    const height = 170;
    const chart = svg("svg", { viewBox: `0 0 ${width} ${height}`, width: "100%", height, role: "img" });
    const maxValue = niceMax(Math.max(1, ...series.flatMap(item => points.map(point => item.value(point)))));
    const plotWidth = width - PAD.left - PAD.right;
    const plotHeight = height - PAD.top - PAD.bottom;
    const x = index => PAD.left + (points.length <= 1 ? plotWidth / 2 : (index / (points.length - 1)) * plotWidth);
    const y = value => PAD.top + plotHeight * (1 - value / maxValue);
    const tickCount = Math.min(points.length, width < 400 ? 3 : 5);
    const labels = [];
    for (let tick = 0; tick < tickCount; tick++) {
      const index = tickCount === 1 ? 0 : Math.round((tick / (tickCount - 1)) * (points.length - 1));
      labels.push({ x: x(index), text: bucketLabel(points[index].t, bucket), anchor: tick === 0 ? "start" : tick === tickCount - 1 ? "end" : "middle" });
    }
    axes(chart, width, height, maxValue, labels, unit);
    for (const item of series) {
      const path = points.map((point, index) => `${index ? "L" : "M"}${x(index).toFixed(1)},${y(item.value(point)).toFixed(1)}`).join("");
      if (series.length === 1 && points.length > 1) {
        chart.append(svg("path", { d: `${path}L${x(points.length - 1)},${y(0)}L${x(0)},${y(0)}Z`, fill: item.color, "fill-opacity": "0.14" }));
      }
      chart.append(svg("path", { d: path, fill: "none", stroke: item.color, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
    }
    const cross = svg("line", { y1: PAD.top, y2: height - PAD.bottom, class: "stats-crosshair", visibility: "hidden" });
    const dots = series.map(item => svg("circle", { r: 4, fill: item.color, stroke: "var(--surface)", "stroke-width": 2, visibility: "hidden" }));
    chart.append(cross, ...dots);
    wrap.append(chart);
    const tip = tooltipLayer(wrap);
    const pick = event => {
      const box = chart.getBoundingClientRect();
      const px = ((event.clientX - box.left) / box.width) * width;
      const index = Math.min(points.length - 1, Math.max(0, Math.round(((px - PAD.left) / plotWidth) * (points.length - 1))));
      const point = points[index];
      cross.setAttribute("x1", x(index));
      cross.setAttribute("x2", x(index));
      cross.setAttribute("visibility", "visible");
      series.forEach((item, seriesIndex) => {
        dots[seriesIndex].setAttribute("cx", x(index));
        dots[seriesIndex].setAttribute("cy", y(item.value(point)));
        dots[seriesIndex].setAttribute("visibility", "visible");
      });
      tip.show((x(index) / width) * box.width, (Math.min(...series.map(item => y(item.value(point)))) / height) * box.height,
        [bucketLabel(point.t, bucket, true), ...series.map(item => `${item.label}: ${format(item.value(point))}`)]);
    };
    chart.addEventListener("pointermove", pick);
    chart.addEventListener("pointerdown", pick);
    chart.addEventListener("pointerleave", () => {
      cross.setAttribute("visibility", "hidden");
      for (const dot of dots) dot.setAttribute("visibility", "hidden");
      tip.hide();
    });
    chart.setAttribute("aria-label", series.map(item => item.label).join(" and ") + " over time");
    wrap.append(dataTable(chart.getAttribute("aria-label"), ["Period", ...series.map(item => item.label)],
      points.map(point => [bucketLabel(point.t, bucket, true), ...series.map(item => format(item.value(point)))])));
    return wrap;
  }

  // Vertical bars, one series.
  function barChart(labels, values, tickEvery, describe) {
    const wrap = make("div", "stats-chart");
    const width = Math.max(260, root.clientWidth - 34);
    const height = 150;
    const chart = svg("svg", { viewBox: `0 0 ${width} ${height}`, width: "100%", height, role: "img" });
    const maxValue = niceMax(Math.max(1, ...values));
    const plotWidth = width - PAD.left - PAD.right;
    const plotHeight = height - PAD.top - PAD.bottom;
    const slot = plotWidth / values.length;
    const barWidth = Math.max(3, Math.min(28, slot - 2));
    const axisLabels = [];
    labels.forEach((label, index) => {
      if (index % tickEvery === 0) axisLabels.push({ x: PAD.left + slot * index + slot / 2, text: label });
    });
    axes(chart, width, height, maxValue, axisLabels);
    const tip = tooltipLayer(wrap);
    values.forEach((value, index) => {
      const barHeight = (value / maxValue) * plotHeight;
      const bx = PAD.left + slot * index + (slot - barWidth) / 2;
      const by = PAD.top + plotHeight - barHeight;
      if (value > 0) {
        const radius = Math.min(4, barWidth / 2, barHeight);
        chart.append(svg("path", {
          d: `M${bx},${by + barHeight}V${by + radius}Q${bx},${by} ${bx + radius},${by}H${bx + barWidth - radius}Q${bx + barWidth},${by} ${bx + barWidth},${by + radius}V${by + barHeight}Z`,
          fill: COLORS.series,
        }));
      }
      const hit = svg("rect", { x: PAD.left + slot * index, y: PAD.top, width: slot, height: plotHeight, fill: "transparent" });
      const show = () => {
        const box = chart.getBoundingClientRect();
        tip.show(((bx + barWidth / 2) / width) * box.width, (by / height) * box.height, describe(index, value));
      };
      hit.addEventListener("pointerenter", show);
      hit.addEventListener("pointerdown", show);
      hit.addEventListener("pointerleave", () => tip.hide());
      chart.append(hit);
    });
    wrap.append(chart);
    return wrap;
  }

  function statTiles(totals, includeUsers) {
    const grid = make("div", "stats-tiles");
    const tiles = [["Plays", totals.plays.toLocaleString()], ["Watch time", duration(totals.watch_seconds)]];
    if (includeUsers) tiles.push(["Users", totals.users.toLocaleString()]);
    tiles.push(["Titles", totals.titles.toLocaleString()]);
    if (!includeUsers) tiles.push(["Devices", totals.devices.toLocaleString()]);
    for (const [label, value] of tiles) {
      const tile = make("div", "stats-tile");
      tile.append(make("strong", "stats-tile-value", value), make("span", "stats-tile-label", label));
      grid.append(tile);
    }
    return grid;
  }

  function methodCard(methods) {
    const node = card("Stream quality");
    const keys = ["direct_play", "direct_stream", "transcode"];
    const total = keys.reduce((sum, key) => sum + (methods[key] || 0), 0);
    if (total === 0) {
      node.append(make("p", "stats-empty", "No plays in this period"));
      return node;
    }
    const bar = make("div", "stats-segments");
    bar.setAttribute("role", "img");
    bar.setAttribute("aria-label", keys.map(key => `${METHOD_LABELS[key]} ${methods[key] || 0}`).join(", "));
    const legend = make("div", "stats-legend stats-legend-figures");
    for (const key of keys) {
      const count = methods[key] || 0;
      const percent = Math.round((count / total) * 100);
      if (count > 0) {
        const segment = make("span", "stats-segment");
        segment.style.flexGrow = String(count);
        segment.style.background = COLORS[key];
        segment.title = `${METHOD_LABELS[key]}: ${count} (${percent}%)`;
        bar.append(segment);
      }
      const item = make("div", "stats-legend-item");
      const swatch = make("span", "stats-swatch");
      swatch.style.background = COLORS[key];
      const label = make("span", "stats-legend-label");
      label.append(swatch, document.createTextNode(METHOD_LABELS[key]));
      item.append(label, make("strong", "stats-legend-value", `${percent}%`), make("span", "stats-legend-count", plural(count, "play")));
      legend.append(item);
    }
    node.append(bar, legend);
    return node;
  }

  function rankedBars(title, items, unit) {
    const node = card(title);
    if (!items?.length) {
      node.append(make("p", "stats-empty", "No plays in this period"));
      return node;
    }
    const top = Math.max(...items.map(item => item.plays));
    const list = make("ol", "stats-meters");
    for (const item of items) {
      const row = make("li", "stats-meter");
      const track = make("span", "stats-meter-track");
      const fill = make("span", "stats-meter-fill");
      fill.style.width = `${(item.plays / top) * 100}%`;
      track.append(fill);
      row.append(make("span", "stats-meter-name", item.name), track, make("span", "stats-meter-value", plural(item.plays, unit)));
      list.append(row);
    }
    node.append(list);
    return node;
  }

  function poster(url, className = "stats-poster") {
    const box = make("span", className);
    box.append(make("span", "ui-icon icon-playing stats-poster-fallback"));
    if (url) {
      const image = document.createElement("img");
      image.alt = "";
      image.loading = "lazy";
      image.referrerPolicy = "no-referrer";
      image.src = url;
      image.addEventListener("error", () => image.remove(), { once: true });
      box.append(image);
    }
    return box;
  }

  function topList(title, items, valueKey, valueLabel, onSelect, showPoster = true) {
    const node = card(title, "stats-top");
    if (!items?.length) {
      node.append(make("p", "stats-empty", "Nothing yet"));
      return node;
    }
    const heading = node.querySelector(".stats-card-title");
    heading.append(make("span", "stats-top-unit", valueLabel));
    const body = make("div", "stats-top-body");
    if (showPoster) body.append(poster(items[0].poster_url));
    const list = make("ol", "stats-top-list");
    items.forEach((item, index) => {
      const row = make("li", index === 0 ? "stats-top-row first" : "stats-top-row");
      const name = make(onSelect ? "button" : "span", "stats-top-name", item.title || item.name);
      if (onSelect) {
        name.type = "button";
        name.addEventListener("click", () => onSelect(item));
      }
      row.append(make("span", "stats-top-rank", String(index + 1)), name, make("span", "stats-top-value", String(item[valueKey])));
      list.append(row);
    });
    body.append(list);
    node.append(body);
    return node;
  }

  function recentList(title, plays, showPerson) {
    const node = card(title);
    if (!plays?.length) {
      node.append(make("p", "stats-empty", "No plays in this period"));
      return node;
    }
    const list = make("ul", "stats-recent");
    for (const play of plays) {
      const row = make("li", "stats-recent-row");
      const copy = make("span", "stats-recent-copy");
      copy.append(make("strong", "", play.title));
      if (play.subtitle) copy.append(make("span", "", play.subtitle));
      const when = new Date(play.started_at).toLocaleString(undefined, { day: "numeric", month: "short", hour: "numeric", minute: "2-digit" });
      copy.append(make("span", "stats-muted", [showPerson ? play.person : "", play.platform, when].filter(Boolean).join(" · ")));
      const meta = make("span", "stats-recent-meta");
      meta.append(make("span", "stats-chip", play.active ? "Watching" : play.watched ? "Watched" : play.percent_complete != null ? `${Math.round(play.percent_complete)}%` : duration(play.watch_seconds)));
      if (play.method) meta.append(make("span", `stats-chip method-${play.method}`, METHOD_LABELS[play.method] || play.method));
      row.append(poster(play.poster_url, "stats-poster small"), copy, meta);
      list.append(row);
    }
    node.append(list);
    return node;
  }

  function overTimeCards(data, includeConcurrency) {
    const cards = [];
    const points = data.plays_over_time || [];
    if (points.length) {
      const plays = card("Plays over time");
      plays.append(lineChart([{ label: "Plays", color: COLORS.series, value: point => point.plays }], points, data.bucket, value => String(value)));
      cards.push(plays);
      const watch = card("Watch time");
      watch.append(lineChart([{ label: "Watch time", color: COLORS.series, value: point => point.watch_seconds / 3600 }], points, data.bucket, value => duration(value * 3600), "h"));
      cards.push(watch);
    }
    if (includeConcurrency && data.concurrent?.length) {
      const concurrent = card("Concurrent streams");
      const legend = make("div", "stats-legend");
      for (const [label, color] of [["Streams", COLORS.series], ["Transcodes", COLORS.transcode]]) {
        const item = make("span", "stats-legend-label");
        const swatch = make("span", "stats-swatch");
        swatch.style.background = color;
        item.append(swatch, document.createTextNode(label));
        legend.append(item);
      }
      const peak = data.peak?.streams ? make("p", "stats-muted stats-peak", `Peak ${data.peak.streams} streams${data.peak.at ? `, ${new Date(data.peak.at).toLocaleString(undefined, { day: "numeric", month: "short", hour: "numeric", minute: "2-digit" })}` : ""} · peak ${data.peak.transcodes} transcodes`) : null;
      concurrent.append(legend, lineChart([
        { label: "Streams", color: COLORS.series, value: point => point.streams },
        { label: "Transcodes", color: COLORS.transcode, value: point => point.transcodes },
      ], data.concurrent, data.bucket, value => String(value)));
      if (peak) concurrent.append(peak);
      cards.push(concurrent);
    }
    const weekday = card("Plays by day");
    weekday.append(barChart(WEEKDAYS, data.by_weekday, 1, (index, value) => [WEEKDAYS[index], plural(value, "play")]));
    const hours = card("Peak hours");
    hours.append(barChart([...Array(24).keys()].map(hourLabel), data.by_hour, 6, (index, value) => [hourLabel(index), plural(value, "play")]));
    cards.push(weekday, hours, methodCard(data.methods || {}));
    return cards;
  }

  function filters() {
    const bar = make("div", "stats-filters");
    const group = (name, options, current, onChange) => {
      const set = make("div", "stats-chips");
      set.setAttribute("role", "group");
      set.setAttribute("aria-label", name);
      for (const [value, label] of options) {
        const button = make("button", value === current ? "stats-filter active" : "stats-filter", label);
        button.type = "button";
        button.setAttribute("aria-pressed", String(value === current));
        button.addEventListener("click", () => onChange(value));
        set.append(button);
      }
      return set;
    };
    if (!state.person) {
      bar.append(group("View", [["overview", "Overview"], ["users", "Users"]], state.view, value => { state.view = value; load(); }));
    }
    bar.append(
      group("Period", RANGES, state.range, value => { state.range = value; load(); }),
      group("Server", SOURCES, state.source, value => { state.source = value; load(); }),
    );
    return bar;
  }

  function openPerson(id) {
    state.person = id;
    state.notice = "";
    load();
    document.querySelector(".page-shell")?.scrollTo(0, 0);
  }

  function renderOverview(data) {
    const content = [statTiles(data.totals, true), ...overTimeCards(data, true)];
    content.push(
      rankedBars("Top platforms", data.platforms, "play"),
      topList("Most watched movies", data.top_movies, "plays", "Plays"),
      topList("Most popular movies", data.popular_movies, "users", "Users"),
      topList("Most watched shows", data.top_shows, "plays", "Plays"),
      topList("Most popular shows", data.popular_shows, "users", "Users"),
      topList("Most active users", (data.top_users || []).map(user => ({ ...user, title: user.name })), "plays", "Plays", user => openPerson(user.person_id), false),
      recentList("Recently watched", data.recent, true),
    );
    return content;
  }

  function renderUsers(users) {
    const content = [];
    const controls = make("div", "stats-user-controls");
    const search = document.createElement("input");
    search.type = "search";
    search.className = "stats-search";
    search.placeholder = "Search users";
    search.setAttribute("aria-label", "Search users");
    search.value = state.query;
    const sortSelect = document.createElement("select");
    sortSelect.className = "stats-sort";
    sortSelect.setAttribute("aria-label", "Sort users");
    for (const [value, label] of [["plays", "Most plays"], ["watch", "Most watch time"], ["active", "Last active"], ["name", "Name"]]) {
      const option = make("option", "", label);
      option.value = value;
      option.selected = value === state.sort;
      sortSelect.append(option);
    }
    controls.append(search, sortSelect);
    content.push(controls);
    const list = make("ul", "stats-users");
    const draw = () => {
      const query = state.query.trim().toLowerCase();
      const sorted = users.filter(user => !query || user.name.toLowerCase().includes(query));
      const sorters = {
        plays: (a, b) => b.plays - a.plays || a.name.localeCompare(b.name),
        watch: (a, b) => b.watch_seconds - a.watch_seconds || a.name.localeCompare(b.name),
        active: (a, b) => (b.last_active || 0) - (a.last_active || 0),
        name: (a, b) => a.name.localeCompare(b.name),
      };
      sorted.sort(sorters[state.sort]);
      list.replaceChildren();
      for (const user of sorted) {
        const row = make("li");
        const button = make("button", "stats-user");
        button.type = "button";
        button.addEventListener("click", () => openPerson(user.person_id));
        const avatar = make("span", "stats-avatar", (user.name || "?").trim().charAt(0).toUpperCase());
        const copy = make("span", "stats-user-copy");
        copy.append(make("strong", "", user.name));
        copy.append(make("span", "stats-muted", `${user.sources.map(source => source === "silo" ? "Silo" : "Plex").join(" + ")} · ${plural(user.devices, "device")} · ${plural(user.titles, "title")}`));
        const figures = make("span", "stats-user-figures");
        figures.append(make("strong", "", plural(user.plays, "play")), make("span", "stats-muted", duration(user.watch_seconds)), make("span", "stats-muted", relative(user.last_active)));
        button.append(avatar, copy, figures);
        row.append(button);
        list.append(row);
      }
      if (!sorted.length) list.append(make("li", "stats-empty", "No users match"));
    };
    search.addEventListener("input", () => { state.query = search.value; draw(); });
    sortSelect.addEventListener("change", () => { state.sort = sortSelect.value; draw(); });
    draw();
    const count = make("p", "stats-muted stats-user-count", `${users.length} user${users.length === 1 ? "" : "s"}`);
    content.push(count, list);
    return content;
  }

  function personHeader(person) {
    const header = make("div", "stats-person-header");
    const back = make("button", "icon-button", "");
    back.type = "button";
    back.setAttribute("aria-label", "Back to users");
    back.title = "Back to users";
    back.append(make("span", "ui-icon icon-back"));
    back.addEventListener("click", () => { state.person = null; state.view = "users"; load(); });
    const rename = make("button", "stats-text-button", "Rename");
    rename.type = "button";
    rename.addEventListener("click", () => {
      const name = window.prompt("Name shown for this person", person.name);
      if (name && name.trim() && name.trim() !== person.name) changePeople({ action: "rename", person_id: person.id, display_name: name.trim() }, person.id);
    });
    header.append(back, make("span", "stats-avatar large", (person.name || "?").charAt(0).toUpperCase()), make("h3", "stats-person-name", person.name), rename);
    return header;
  }

  // changePeople links, unlinks or renames, then shows the person who now
  // holds the account in question (linking can move to the other person).
  async function changePeople(body, followAccountOrPerson, followAccount = false) {
    state.notice = "";
    try {
      const response = await fetch("/api/people", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), signal: AbortSignal.timeout(10000) });
      const payload = await response.json();
      if (!response.ok) throw new Error(payload?.error || "The change could not be saved");
      state.people = payload;
      const target = followAccount
        ? payload.find(person => person.accounts.some(account => account.id === followAccountOrPerson))
        : payload.find(person => person.id === followAccountOrPerson);
      state.notice = { link: "Accounts linked", unlink: "Account unlinked", rename: "Name saved" }[body.action];
      if (target) state.person = target.id;
      load();
    } catch (error) {
      state.notice = error.message || "The change could not be saved";
      render();
    }
  }

  function renderPerson(data) {
    const person = data.person;
    const accounts = card("Accounts");
    const accountList = make("ul", "stats-accounts");
    for (const account of person.accounts) {
      const row = make("li", "stats-account");
      const copy = make("span", "stats-recent-copy");
      copy.append(make("strong", "", `${account.source === "silo" ? "Silo" : "Plex"} · ${account.display_name || account.username || account.external_id}`));
      const details = [account.email, account.plex_identity ? "Plex identity" : "", account.link_method === "manual" ? "Linked by hand" : ""].filter(Boolean).join(" · ");
      if (details) copy.append(make("span", "stats-muted", details));
      const side = make("span", "stats-account-side");
      side.append(make("span", "stats-muted", `Last active ${relative(account.last_seen)}`));
      if (person.accounts.length > 1) {
        const unlink = make("button", "stats-text-button", "Unlink");
        unlink.type = "button";
        unlink.setAttribute("aria-label", `Unlink ${account.source === "silo" ? "Silo" : "Plex"} account ${account.display_name || account.username || account.external_id}`);
        // The rest of this person stays on this page; the unlinked account becomes its own person.
        const remaining = person.accounts.find(other => other.id !== account.id);
        unlink.addEventListener("click", () => changePeople({ action: "unlink", account_id: account.id }, remaining.id, true));
        side.append(unlink);
      }
      row.append(copy, side);
      accountList.append(row);
    }
    accounts.append(accountList);
    const candidates = state.people
      .filter(other => other.id !== person.id)
      .flatMap(other => other.accounts.map(account => ({ person: other, account })))
      .sort((a, b) => a.person.display_name.localeCompare(b.person.display_name));
    if (candidates.length) {
      const form = make("div", "stats-link-form");
      const select = document.createElement("select");
      select.className = "stats-sort";
      select.setAttribute("aria-label", "Account to link to this person");
      const placeholder = make("option", "", "Link another account…");
      placeholder.value = "";
      select.append(placeholder);
      for (const { person: other, account } of candidates) {
        const option = make("option", "", `${other.display_name} — ${account.source === "silo" ? "Silo" : "Plex"} · ${account.username || account.display_name || account.external_id}${account.email ? ` · ${account.email}` : ""}`);
        option.value = String(account.id);
        select.append(option);
      }
      const link = make("button", "stats-filter", "Link");
      link.type = "button";
      link.disabled = true;
      select.addEventListener("change", () => { link.disabled = !select.value; });
      link.addEventListener("click", () => {
        const chosen = Number(select.value);
        if (chosen) changePeople({ action: "link", account_ids: [person.accounts[0].id, chosen] }, person.accounts[0].id, true);
      });
      form.append(select, link);
      accounts.append(form);
    }
    if (state.notice) accounts.append(make("p", "stats-muted stats-notice", state.notice));
    const devices = card("Devices");
    if (data.devices?.length) {
      const list = make("ul", "stats-accounts");
      for (const device of data.devices) {
        const row = make("li", "stats-account");
        const copy = make("span", "stats-recent-copy");
        copy.append(make("strong", "", device.name), make("span", "stats-muted", [device.platform, device.source === "silo" ? "Silo" : "Plex", plural(device.plays, "play")].filter(Boolean).join(" · ")));
        row.append(copy, make("span", "stats-muted", relative(device.last_seen)));
        list.append(row);
      }
      devices.append(list);
    } else devices.append(make("p", "stats-empty", "No devices in this period"));
    const addresses = card("IP addresses");
    if (data.addresses?.length) {
      const list = make("ul", "stats-accounts");
      for (const address of data.addresses) {
        const row = make("li", "stats-account");
        const copy = make("span", "stats-recent-copy");
        copy.append(make("strong", "", address.ip), make("span", "stats-muted", [address.location ? address.location.toUpperCase() : "", plural(address.plays, "play")].filter(Boolean).join(" · ")));
        row.append(copy, make("span", "stats-muted", relative(address.last_seen)));
        list.append(row);
      }
      addresses.append(list);
    } else addresses.append(make("p", "stats-empty", "No addresses in this period"));
    return [
      statTiles(data.totals, false),
      accounts,
      ...overTimeCards(data, false),
      rankedBars("Platforms", data.platforms, "play"),
      topList("Most watched shows", data.top_shows, "plays", "Plays"),
      topList("Most watched movies", data.top_movies, "plays", "Plays"),
      devices,
      addresses,
      recentList("Recent plays", data.recent, false),
    ];
  }

  function render() {
    const content = [];
    if (state.person && state.data?.person) content.push(personHeader(state.data.person));
    content.push(filters());
    if (state.error) content.push(make("p", "inline-message", state.error));
    else if (state.loading && !state.data && !state.users) content.push(make("p", "stats-muted", "Loading stats…"));
    else if (state.person && state.data?.person) content.push(...renderPerson(state.data));
    else if (state.view === "users" && state.users) content.push(...renderUsers(state.users));
    else if (state.view === "overview" && state.data) content.push(...renderOverview(state.data));
    root.replaceChildren(...content);
    root.setAttribute("aria-busy", String(state.loading));
  }

  async function load() {
    const id = ++request;
    state.loading = true;
    state.error = "";
    render();
    const params = new URLSearchParams({ range: state.range, source: state.source, tz: zone });
    try {
      if (state.view === "users" && !state.person) {
        const response = await fetch(`/api/stats/users?${params}`, { cache: "no-store", signal: AbortSignal.timeout(20000) });
        if (!response.ok) throw new Error();
        const users = await response.json();
        if (id !== request) return;
        state.users = users;
      } else {
        if (state.person) params.set("person", state.person);
        const [response, people] = await Promise.all([
          fetch(`/api/stats?${params}`, { cache: "no-store", signal: AbortSignal.timeout(20000) }),
          state.person ? fetch("/api/people", { cache: "no-store", signal: AbortSignal.timeout(20000) }).then(reply => reply.ok ? reply.json() : []) : Promise.resolve(state.people),
        ]);
        if (!response.ok) throw new Error();
        const data = await response.json();
        if (id !== request) return;
        state.data = data;
        state.people = Array.isArray(people) ? people : [];
      }
    } catch {
      if (id !== request) return;
      state.error = "Stats are unavailable. Check that history storage is configured.";
    }
    state.loading = false;
    render();
  }

  let loadedOnce = false;
  window.addEventListener("monitor-view-change", () => {
    if (root.closest("[data-view]")?.hidden) return;
    if (!loadedOnce) {
      loadedOnce = true;
      load();
    } else render();
  });
  window.addEventListener("monitor-theme-change", () => {
    readColors();
    if (loadedOnce && !root.closest("[data-view]")?.hidden) render();
  });
  let resizeFrame = 0;
  window.addEventListener("resize", () => {
    if (root.closest("[data-view]")?.hidden) return;
    window.cancelAnimationFrame(resizeFrame);
    resizeFrame = window.requestAnimationFrame(render);
  });
  return { load };
})();
