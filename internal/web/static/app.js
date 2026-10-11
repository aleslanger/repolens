// repolens dashboard core: data access, DOM helpers, router, portfolio and status pages.
(function () {
  "use strict";
  const C = window.Charts;
  const I = window.I18N;
  const t = I.t;
  const bundleEl = document.getElementById("bundle");
  const BUNDLE = bundleEl ? JSON.parse(bundleEl.textContent) : null;
  const DAY = 86400;
  const RESIZE_DEBOUNCE_MS = 150;
  const ACTIVE_DAYS = 30;

  // ---- data ---------------------------------------------------------------
  async function api(path) {
    if (BUNDLE) {
      path = path.split("?")[0]; // the export holds the all-time view only
      const m = path.match(/^\/api\/repo\/(\d+)$/);
      if (m) return BUNDLE.repos[m[1]] || null;
      const pm = path.match(/^\/api\/person\/(\d+)$/);
      if (pm) return (BUNDLE.persons || {})[pm[1]] || null;
      const sm = path.match(/^\/api\/session\/(.+)$/);
      if (sm) return (BUNDLE.sessions || {})[decodeURIComponent(sm[1])] || null;
      const key = { "/api/portfolio": "portfolio", "/api/agents": "agents", "/api/people": "people", "/api/status": "status",
        "/api/attention": "attention", "/api/summary": "summary" }[path];
      return BUNDLE[key] === undefined ? null : BUNDLE[key];
    }
    const res = await fetch(path, { headers: { Accept: "application/json" } });
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
    return res.json();
  }

  // ---- DOM helpers (textContent only; no HTML injection) -------------------
  function h(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const k in attrs || {}) {
      const v = attrs[k];
      if (v == null || v === false) continue;
      if (k === "class") e.className = v;
      else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
      else if (k === "html") e.innerHTML = v; // only for trusted, pre-escaped markup (sparklines)
      else e.setAttribute(k, v === true ? "" : v);
    }
    for (const kid of kids.flat()) {
      if (kid == null || kid === false) continue;
      e.appendChild(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return e;
  }

  const nf = C.nf;
  const fmt = (v) => (v == null ? "–" : nf.format(v));
  const usd = (v) => (v == null ? "–" : "$" + (v >= 100 ? nf.format(Math.round(v)) : C.nf1(v, 2)));
  const fmtDate = (ts) => (ts ? new Date(ts * 1000).toLocaleDateString(I.locale(), { day: "numeric", month: "numeric", year: "numeric" }) : "–");
  const fmtDateTime = (ts) => (ts ? new Date(ts * 1000).toLocaleString(I.locale()) : "–");
  const HOUR = 3600;
  function relTime(ts) {
    if (!ts) return "–";
    const s = Date.now() / 1000 - ts;
    if (s < HOUR) return t("před chvílí");
    if (s < DAY) return I.plural(Math.floor(s / HOUR), ["před {n} hodinou", "před {n} hodinami", "před {n} hodinami"]);
    const d = Math.floor(s / DAY);
    if (d === 1) return t("včera");
    if (d < 30) return I.plural(d, ["před {n} dnem", "před {n} dny", "před {n} dny"]);
    if (d < 365) return t("před {n} měs.", { n: Math.round(d / 30) });
    return t("před {n} r.", { n: C.nf1(d / 365, 1) });
  }
  function hours(hrs) {
    if (hrs == null) return "–";
    if (hrs < 48) return `${Math.round(hrs)} h`;
    return `${Math.round(hrs / 24)} d`;
  }
  function duration(ms) {
    const m = Math.round(ms / 60000);
    if (m < 60) return `${m} min`;
    return `${C.nf1(m / 60, 1)} h`;
  }

  function sourceBadges(r) {
    return h("span", { class: "badges" },
      r.inLocal ? h("span", { class: "badge src-git", title: t("statistiky z lokálního git log") }, "git") : null,
      r.inGithub ? h("span", { class: "badge src-gh", title: t("issues, PR, CI z GitHub API") }, "GitHub") : null,
      !r.inGithub && !r.key.startsWith("local:") ? h("span", { class: "badge", title: t("GitHub data nestažena") }, t("bez API")) : null,
      r.key.startsWith("local:") ? h("span", { class: "badge", title: t("repo bez remote") }, t("jen lokální")) : null,
      r.fork ? h("span", { class: "badge", title: t("fork cizího repozitáře") }, "fork") : null,
      r.archived ? h("span", { class: "badge", title: t("archivováno na GitHubu") }, t("archiv")) : null);
  }

  // Collectors store a few Czech markers inside names ("/clear (vestavěný)"); translate them.
  const MARKERS = ["(vestavěný)", "(neznámý)", "(neplatné jméno)", "(ostatní)", "(další klon)", "(skryto)"];
  function localName(name) {
    let out = String(name);
    for (const m of MARKERS) if (out.includes(m)) out = out.replace(m, t(m));
    return out;
  }

  function ciStatus(c) {
    if (!c) return h("span", { class: "status none" }, t("bez CI"));
    if (c === "success") return h("span", { class: "status good" }, t("prošlo"));
    if (["failure", "timed_out", "startup_failure"].includes(c)) return h("span", { class: "status critical" }, t("selhalo"));
    return h("span", { class: "status warning" }, c.replace(/_/g, " "));
  }

  function tile(label, value, note) {
    return h("div", { class: "tile" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), note ? h("div", { class: "note" }, note) : null);
  }

  function section(num, title) {
    return h("h2", {}, h("span", { class: "num" }, String(num).padStart(2, "0")), title);
  }

  // ---- chart cards with a table view and resize redraw -----------------------
  const redraws = new Set();
  let resizeTimer = 0;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => redraws.forEach((f) => f()), RESIZE_DEBOUNCE_MS);
  });

  // opts: {title, note, span, draw(el), table:{head:[...], rows:[[...]] , num:[colIdx]}, tools}
  function chartCard(opts) {
    const body = h("div", { "data-label": opts.title });
    let mode = "chart";
    const render = () => {
      if (!body.isConnected) return redraws.delete(render);
      if (mode === "chart") opts.draw(body);
      else body.replaceChildren(dataTable(opts.table.head, opts.table.rows, opts.table.num || []));
    };
    const seg = opts.table ? segmented([["chart", t("Graf")], ["table", t("Tabulka")]], mode, (m) => { mode = m; render(); }) : null;
    const csv = opts.table ? csvButton(opts.title, () => opts.table.head, () => opts.table.rows) : null;
    const card = h("section", { class: `card span-${opts.span || 12}` },
      h("div", { class: "card-head" }, h("div", {}, h("h3", {}, opts.title), opts.note ? h("p", {}, opts.note) : null),
        h("div", { class: "card-tools" }, opts.tools || null, seg, csv)),
      body);
    card.rerender = render;
    redraws.add(render);
    requestAnimationFrame(render);
    return card;
  }

  function segmented(options, current, onChange) {
    const wrap = h("span", { class: "seg", role: "group" });
    for (const [value, label] of options) {
      const b = h("button", { type: "button", "aria-pressed": String(value === current) }, label);
      b.addEventListener("click", () => {
        wrap.querySelectorAll("button").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
        onChange(value);
      });
      wrap.appendChild(b);
    }
    return wrap;
  }

  // csvCell quotes one CSV cell. Text that a spreadsheet would run as a formula (=, +, -, @, tab,
  // carriage return at the start; issue titles and commit subjects come from anyone) gets a
  // leading apostrophe so it stays text. Numbers stay numbers.
  function csvCell(c) {
    if (typeof c === "number") return String(c);
    let s = c instanceof Node ? c.textContent : c == null ? "" : String(c);
    if (/^[=+\-@\t\r]/.test(s)) s = "'" + s;
    return /[",\n\r;]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  }

  // csvButton downloads a table as CSV. Cells that are elements contribute their text.
  function csvButton(name, head, rows) {
    const cell = csvCell;
    return h("button", { type: "button", class: "btn", title: t("Stáhnout jako CSV"), onclick: () => {
      const text = [head(), ...rows()].map((r) => r.map(cell).join(",")).join("\n");
      const a = h("a", { href: URL.createObjectURL(new Blob(["\ufeff" + text], { type: "text/csv;charset=utf-8" })),
        download: (name || "repolens").replace(/[^\p{L}\p{N}]+/gu, "-").toLowerCase() + ".csv" });
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(a.href);
    } }, "CSV");
  }

  // dataTable renders a table; with csvName it gets a CSV download button above it.
  function dataTable(head, rows, numCols, csvName) {
    const num = new Set(numCols);
    const table = tableEl(head, rows, num);
    if (!csvName) return table;
    return h("div", { class: "table-block" }, h("div", { class: "table-tools" }, csvButton(csvName, () => head, () => rows)), table);
  }

  function tableEl(head, rows, num) {
    return h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, head.map((c, i) => h("th", { class: num.has(i) ? "num" : null }, c)))),
      h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c, i) => h("td", { class: num.has(i) ? "num" : null }, c instanceof Node ? c : c == null ? "–" : typeof c === "number" ? fmt(c) : c)))))));
  }

  function empty(text) {
    return h("p", { class: "empty" }, text);
  }

  // info is a small "i" with an explanation, reachable by keyboard and screen readers.
  function info(text) {
    return h("span", { class: "info", tabindex: "0", role: "note", title: text, "aria-label": text }, "i");
  }

  // ---- page tabs and the period switch, both kept in the URL ----------------------------------
  function tabs(defs, current, params) {
    const nav = h("div", { class: "tabs" });
    for (const [key, label] of defs) {
      nav.appendChild(h("button", { type: "button", "aria-pressed": String(key === current), onclick: () => {
        const p = new URLSearchParams(params);
        if (key === defs[0][0]) p.delete("tab"); else p.set("tab", key);
        navigate(p);
      } }, label));
    }
    return nav;
  }
  function tabOf(defs, params) {
    return defs.some(([k]) => k === params.get("tab")) ? params.get("tab") : defs[0][0];
  }

  const PERIODS = [["30", "30 dní"], ["90", "90 dní"], ["365", "rok"], ["all", "vše"]];
  function daysOf(params) {
    return PERIODS.some(([k]) => k === params.get("days")) ? params.get("days") : "all";
  }
  // daysQuery is the API query for the chosen period ("" for everything).
  const daysQuery = (days) => (days === "all" ? "" : "?days=" + days);

  // periodControl switches the period of the current page; the static export has none.
  function periodControl(params, note) {
    if (BUNDLE) return null;
    const days = daysOf(params);
    const control = segmented(PERIODS.map(([k, l]) => [k, t(l)]), days, (k) => {
      const p = new URLSearchParams(params);
      if (k === "all") p.delete("days"); else p.set("days", k);
      navigate(p);
    });
    control.setAttribute("aria-label", t("Období:"));
    return h("div", { class: "toolbar period" }, h("span", { class: "note", style: "margin:0" }, t("Období:")), control,
      days !== "all" && note ? h("span", { class: "note", style: "margin:0" }, note) : null);
  }

  // delta compares a value with the previous period: "+12 % proti předchozímu období".
  function delta(cur, prev) {
    if (prev == null) return null;
    if (!prev) return cur ? t("nové proti předchozímu období") : null;
    const pct = Math.round(((cur - prev) / prev) * 100);
    const cls = pct > 0 ? "up" : pct < 0 ? "down" : "flat";
    return h("span", { class: "delta " + cls }, t("{sign}{pct} % proti předchozímu období", { sign: pct > 0 ? "+" : "", pct }));
  }

  function navigate(params) {
    const { parts } = parseHash();
    const q = params.toString();
    location.hash = `#/${parts.join("/")}${q ? "?" + q : ""}`;
  }

  // send makes a state-changing request with the CSRF token of the served page.
  const csrfMeta = document.querySelector('meta[name="repolens-csrf"]');
  async function send(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: { "Content-Type": "application/json", "X-Repolens-CSRF": csrfMeta ? csrfMeta.content : "", Accept: "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
    return { status: res.status, ok: res.ok, data, text };
  }
  // problems turns a 422 answer into translated messages.
  const problems = (r) => (r.data && r.data.errors ? r.data.errors.map((m) => t(m.text, m.args)) : [r.text.trim() || "HTTP " + r.status]);

  // ---- time bucketing ---------------------------------------------------------
  const BUCKET_DAY_LIMIT = 120;
  const BUCKET_WEEK_LIMIT = 3 * 365;

  function parseDay(s) {
    const [y, m, d] = s.split("-").map(Number);
    return new Date(y, m - 1, d);
  }
  function isoDay(d) {
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  }
  function bucketKey(day, unit) {
    const d = parseDay(day);
    if (unit === "day") return day;
    if (unit === "month") return day.slice(0, 7);
    const monday = new Date(d);
    monday.setDate(d.getDate() - ((d.getDay() + 6) % 7));
    return isoDay(monday);
  }
  // Returns ordered bucket keys covering [first,last] for the chosen unit.
  function bucketRange(first, last, unit) {
    const keys = [];
    const d = parseDay(bucketKey(first, unit === "month" ? "day" : unit));
    if (unit === "month") d.setDate(1);
    const end = parseDay(last);
    while (d <= end) {
      keys.push(unit === "month" ? isoDay(d).slice(0, 7) : isoDay(d));
      if (unit === "day") d.setDate(d.getDate() + 1);
      else if (unit === "week") d.setDate(d.getDate() + 7);
      else d.setMonth(d.getMonth() + 1);
    }
    return keys;
  }
  function autoUnit(first, last) {
    const span = (parseDay(last) - parseDay(first)) / 864e5;
    if (span <= BUCKET_DAY_LIMIT) return "day";
    if (span <= BUCKET_WEEK_LIMIT) return "week";
    return "month";
  }
  const UNIT_LABEL = { day: "dny", week: "týdny", month: "měsíce" };

  // ---- commit type groups (max 8 categorical slots) ------------------------------
  const TYPE_GROUPS = [
    { key: "feat", label: "feat", color: "var(--series-1)", types: ["feat"] },
    { key: "fix", label: "fix", color: "var(--series-2)", types: ["fix"] },
    { key: "refactor", label: "refactor/perf", color: "var(--series-3)", types: ["refactor", "perf"] },
    { key: "docs", label: "docs", color: "var(--series-4)", types: ["docs"] },
    { key: "test", label: "test", color: "var(--series-5)", types: ["test"] },
    { key: "chore", label: "chore/ci/build/style", color: "var(--series-6)", types: ["chore", "ci", "build", "style"] },
    { key: "other", label: "ostatní (merge, revert, bez typu)", color: "var(--series-7)", types: ["other", "merge", "revert"] },
  ];
  const typeGroupOf = {};
  TYPE_GROUPS.forEach((g) => g.types.forEach((t) => (typeGroupOf[t] = g.key)));

  // ---- theme ------------------------------------------------------------------
  const THEMES = ["auto", "light", "dark"];
  function storageGet(k) {
    try { return localStorage.getItem(k); } catch (e) { return null; }
  }
  function storageSet(k, v) {
    try { localStorage.setItem(k, v); } catch (e) { /* storage unavailable: theme stays per page view */ }
  }
  function applyTheme(t) {
    if (t === "auto") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", t);
    document.getElementById("theme").textContent = I.t("Motiv: {name}", { name: I.t({ auto: "auto", light: "světlý", dark: "tmavý" }[t]) });
    requestAnimationFrame(() => redraws.forEach((f) => f()));
  }
  let theme = THEMES.includes(storageGet("repolens-theme")) ? storageGet("repolens-theme") : "auto";
  document.getElementById("theme").addEventListener("click", () => {
    theme = THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length];
    storageSet("repolens-theme", theme);
    applyTheme(theme);
  });
  applyTheme(theme);

  // ---- language -------------------------------------------------------------------
  const langBtn = document.getElementById("lang");
  const langMenu = document.getElementById("lang-menu");
  function closeLangMenu() {
    langMenu.hidden = true;
    langBtn.setAttribute("aria-expanded", "false");
  }
  function showLang() {
    const cs = I.lang() === "cs";
    langBtn.querySelector(".flag-cs").hidden = !cs;
    langBtn.querySelector(".flag-en").hidden = cs;
    document.getElementById("lang-code").textContent = cs ? "CZ" : "EN";
    langBtn.setAttribute("aria-label", `${t("Přepnout jazyk")}: ${cs ? "Čeština" : "English"}`);
    langMenu.querySelectorAll("button[data-lang]").forEach((button) => {
      button.setAttribute("aria-pressed", String(button.dataset.lang === I.lang()));
    });
  }
  langBtn.addEventListener("click", () => {
    const open = langMenu.hidden;
    langMenu.hidden = !open;
    langBtn.setAttribute("aria-expanded", String(open));
  });
  langMenu.querySelectorAll("button[data-lang]").forEach((button) => button.addEventListener("click", () => {
    I.setLang(button.dataset.lang);
    closeLangMenu();
    langBtn.focus();
  }));
  document.addEventListener("click", (event) => {
    if (!event.target.closest(".lang-picker")) closeLangMenu();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && !langMenu.hidden) {
      closeLangMenu();
      langBtn.focus();
    }
  });
  I.onChange(() => {
    I.translateStatic();
    showLang();
    applyTheme(theme);
    refreshLastRun();
    route();
  });
  I.translateStatic();
  showLang();

  // ---- last collection in the header ----------------------------------------------------
  async function refreshLastRun() {
    const el = document.getElementById("lastrun");
    try {
      const last = BUNDLE ? (BUNDLE.status && BUNDLE.status.runs[0]) || null : await (await fetch("/api/lastrun", { headers: { Accept: "application/json" } })).json();
      const when = last && (last.finishedAt || last.startedAt);
      el.textContent = when ? t("Poslední sběr {when}", { when: relTime(when) }) : t("Zatím bez sběru");
      el.title = when ? fmtDateTime(when) : "";
    } catch (err) {
      el.textContent = "";
    }
  }

  // ---- router -----------------------------------------------------------------
  const pages = {};
  function parseHash() {
    const raw = location.hash.replace(/^#\/?/, "");
    const [path, query] = raw.split("?");
    return { parts: path.split("/").filter(Boolean), params: new URLSearchParams(query || "") };
  }
  function setParams(params) {
    const { parts } = parseHash();
    const q = params.toString();
    history.replaceState(null, "", `#/${parts.join("/")}${q ? "?" + q : ""}`);
  }
  // NAV_OF maps detail pages to the navigation item they belong to.
  const NAV_OF = { repo: "portfolio", person: "people", session: "agents" };
  async function route() {
    const { parts, params } = parseHash();
    const name = parts[0] || "portfolio";
    const page = pages[name] || pages.portfolio;
    document.querySelectorAll(".nav a").forEach((a) => a.toggleAttribute("aria-current", a.dataset.route === (NAV_OF[name] || name)));
    if (document.querySelector(".nav a[aria-current]")) document.querySelector(".nav a[aria-current]").setAttribute("aria-current", "page");
    document.getElementById("topbar-section").textContent = document.querySelector('.nav a[aria-current="page"]')?.textContent || t("Portfolio");
    const app = document.getElementById("app");
    redraws.clear();
    app.replaceChildren(h("p", { class: "loading" }, t("Načítám…")));
    try {
      const view = await page(parts.slice(1), params);
      // Pages may leave optional parts as null or nested arrays.
      app.replaceChildren(...[].concat(view).flat(Infinity).filter((x) => x != null && x !== false), h("div", { class: "foot-gap" }), footer());
      window.scrollTo(0, 0);
    } catch (err) {
      app.replaceChildren(h("div", { class: "notice" }, t("Data se nepodařilo načíst: {err}", { err: err.message })));
    }
  }
  function footer() {
    let when = BUNDLE ? t("Statický export z {when}.", { when: fmtDateTime(BUNDLE.generatedAt) }) : t("Živá data z lokální databáze.");
    if (BUNDLE && BUNDLE.anonymized) when += " " + t("Anonymizováno: jména repozitářů, lidí, e-maily, cesty a texty commitů jsou nahrazené.");
    return h("p", { class: "foot" }, when + " " + t("Vše běží lokálně; z transcriptů se ukládají jen agregáty a jména."));
  }
  window.addEventListener("hashchange", route);

  // ---- portfolio ------------------------------------------------------------------
  // Labels are Czech source strings, translated where shown.
  const SORTS = {
    activity: { label: "aktivita (26 týdnů)", fn: (a, b) => sum(b.spark) - sum(a.spark) || b.lastCommit - a.lastCommit },
    recent: { label: "poslední commit", fn: (a, b) => b.lastCommit - a.lastCommit },
    commits: { label: "počet commitů", fn: (a, b) => b.commits - a.commits },
    agents: { label: "podíl agentů", fn: (a, b) => b.agentShare - a.agentShare || b.sessions - a.sessions },
    tokens: { label: "tokeny agentů", fn: (a, b) => b.tokens - a.tokens },
    name: { label: "název", fn: (a, b) => a.name.localeCompare(b.name, I.lang()) },
  };
  const SOURCES = { all: "všechny zdroje", local: "lokální git", github: "GitHub", localonly: "jen lokální (bez remote)", nolocal: "jen GitHub (bez klonu)" };
  const sum = (a) => a.reduce((x, y) => x + y, 0);

  function matchSource(r, src) {
    switch (src) {
      case "local": return r.inLocal;
      case "github": return r.inGithub;
      case "localonly": return r.key.startsWith("local:");
      case "nolocal": return !r.inLocal;
      default: return true;
    }
  }

  pages.portfolio = async (_, params) => {
    const days = daysOf(params);
    const [rows, summary, attention] = await Promise.all([
      api("/api/portfolio" + daysQuery(days)).then((r) => r || []),
      api("/api/summary" + daysQuery(days)),
      api("/api/attention").then((r) => r || []),
    ]);
    const now = Date.now() / 1000;
    const active = rows.filter((r) => r.lastCommit > now - ACTIVE_DAYS * DAY).length;
    const openIssues = sum(rows.map((r) => r.openIssues || 0));
    const openPRs = sum(rows.map((r) => r.openPrs || 0));
    const cur = summary ? summary.current : { commits: 0, sessions: 0, fresh: 0, tokens: 0 };
    const prev = summary && summary.previous;
    const inPeriod = days === "all" ? t("celkem") : t("za {period}", { period: t(PERIODS.find(([k]) => k === days)[1]) });

    const state = {
      q: params.get("q") || "",
      sort: SORTS[params.get("sort")] ? params.get("sort") : "activity",
      src: SOURCES[params.get("src")] ? params.get("src") : "all",
      archived: params.get("archived") === "1",
      forks: params.get("forks") !== "0",
    };
    const tbody = h("tbody", {});
    const count = h("span", { class: "count" });
    const draw = () => {
      const q = state.q.toLowerCase();
      const list = rows
        .filter((r) => (state.archived || !r.archived) && (state.forks || !r.fork) && matchSource(r, state.src))
        .filter((r) => !q || (r.name + " " + r.key + " " + r.description + " " + r.language).toLowerCase().includes(q))
        .sort(SORTS[state.sort].fn);
      tbody.replaceChildren(...list.map(portfolioRow));
      count.textContent = t("{n} z {total}", { n: fmt(list.length), total: fmt(rows.length) });
      const p = new URLSearchParams();
      if (days !== "all") p.set("days", days);
      if (state.q) p.set("q", state.q);
      if (state.sort !== "activity") p.set("sort", state.sort);
      if (state.src !== "all") p.set("src", state.src);
      if (state.archived) p.set("archived", "1");
      if (!state.forks) p.set("forks", "0");
      setParams(p);
    };
    const search = h("input", { type: "search", placeholder: t("Hledat repozitář, jazyk…"), value: state.q, "aria-label": t("Hledat") });
    search.addEventListener("input", () => { state.q = search.value; draw(); });
    const sortSel = h("select", { "aria-label": t("Řazení") }, Object.entries(SORTS).map(([k, s]) => h("option", { value: k, selected: k === state.sort }, t("Řadit: {by}", { by: t(s.label) }))));
    sortSel.addEventListener("change", () => { state.sort = sortSel.value; draw(); });
    const srcSel = h("select", { "aria-label": t("Zdroj") }, Object.entries(SOURCES).map(([k, s]) => h("option", { value: k, selected: k === state.src }, t(s))));
    srcSel.addEventListener("change", () => { state.src = srcSel.value; draw(); });
    const forks = h("label", { class: "badge" }, h("input", { type: "checkbox", checked: state.forks, onchange: (e) => { state.forks = e.target.checked; draw(); } }), " " + t("forky"));
    const arch = h("label", { class: "badge" }, h("input", { type: "checkbox", checked: state.archived, onchange: (e) => { state.archived = e.target.checked; draw(); } }), " " + t("archivované"));

    draw();
    return [
      h("div", { class: "hero" },
        h("div", {}, h("h1", { class: "kicker" }, t("Portfolio")), h("div", { class: "hero-figure" }, fmt(rows.length), h("small", {}, t("repozitářů · {active} aktivních za {days} dní", { active: fmt(active), days: ACTIVE_DAYS })))),
        h("div", { class: "tiles" },
          tile(t("Commity") + " · " + inPeriod, fmt(cur.commits), delta(cur.commits, prev && prev.commits)),
          tile(t("Otevřené issues / PR"), rows.some((r) => r.inGithub) ? `${fmt(openIssues)} / ${fmt(openPRs)}` : "–", t("aktuálně, z GitHub API")),
          tile(t("Sessions agentů") + " · " + inPeriod, fmt(cur.sessions), delta(cur.sessions, prev && prev.sessions)),
          tile([t("Tokeny agentů"), " ", info(t("Vstup + výstup. Čtení a zápis cache se počítají zvlášť: opakují stejný kontext a v součtu by přehlušily skutečnou práci."))],
            C.compact(cur.fresh), [t("+ {cache} z cache", { cache: C.compact(cur.tokens - cur.fresh) }), " ", delta(cur.fresh, prev && prev.fresh)]))),
      periodControl(params, t("Commity, sessions a tokeny za zvolené období; aktivita, CI a podíl agentů mají vlastní okna.")),
      attentionPanel(attention),
      rows.length ? null : h("div", { class: "notice" }, t("Zatím nejsou sebraná žádná data. Spusť sběr na stránce Sběr dat nebo příkazem `repolens collect`.")),
      h("div", { class: "toolbar" }, search, sortSel, srcSel, rows.some((r) => r.fork) ? forks : null, arch, count),
      h("div", { class: "table-wrap" }, h("table", {},
        h("thead", {}, h("tr", {},
          h("th", {}, t("Repozitář")), h("th", { class: "hide-sm" }, t("Zdroje")), h("th", {}, t("Aktivita · 26 týdnů")),
          h("th", {}, t("Poslední commit")), h("th", { class: "num" }, t("Commity")), h("th", { class: "num hide-sm" }, "Issues / PR"),
          h("th", { class: "hide-sm" }, "CI"), h("th", {}, t("Agenti · 90 dní"), " ", info(AGENT_SHARE_HELP())), h("th", { class: "num hide-sm" }, t("Tokeny")))),
        tbody)),
    ];
  };

  // AGENT_SHARE_HELP explains the agent share wherever it is shown.
  const AGENT_SHARE_HELP = () => t("Podíl commitů za posledních 90 dní, které mají agentní trailer (Co-Authored-By apod.) nebo vznikly během aktivní session agenta ve stejném repu.");

  const SEVERITY_CLASS = { critical: "critical", warning: "warning", info: "none" };
  function attentionPanel(items) {
    if (!items.length) return h("section", { class: "attention ok" }, h("span", { class: "status good" }, t("Nic nevyžaduje pozornost.")));
    return h("section", { class: "attention", "aria-label": t("Co vyžaduje pozornost") },
      h("h2", { class: "attention-title" }, t("Co vyžaduje pozornost")),
      h("ul", {}, groupAttention(items).map((g) => {
        const it = g[0];
        const mark = h("span", { class: "status " + (SEVERITY_CLASS[it.severity] || "none") }, "");
        if (g.length > 1 && GROUP_TITLE[it.kind]) {
          // One line per kind, with a link to each repository.
          return h("li", {}, mark, h("span", {}, t(GROUP_TITLE[it.kind], it.args), " ",
            ...g.flatMap((x, i) => [i ? ", " : "", h("a", { href: x.link }, localName(x.args.repo) + (x.args.n ? ` (${fmt(x.args.n)})` : ""))])));
        }
        const args = Object.assign({}, it.args);
        if (typeof args.tokens === "number") args.tokens = C.compact(args.tokens);
        if (typeof args.repo === "string") args.repo = localName(args.repo);
        return h("li", {}, mark, h("a", { href: it.link }, t(it.text, args)));
      })));
  }

  // Per-repository kinds are grouped into one line when there are several.
  const GROUP_TITLE = { stale_pr: "Otevřené PR starší než {days} dní:", ci: "Selhané CI:" };
  function groupAttention(items) {
    const groups = [];
    const byKind = {};
    for (const it of items) {
      if (GROUP_TITLE[it.kind] && byKind[it.kind]) byKind[it.kind].push(it);
      else groups.push((byKind[it.kind] = [it]));
    }
    return groups;
  }

  // freshness says when a repository's data was last read, from git and from GitHub.
  function freshness(r) {
    const parts = [];
    if (r.inLocal) parts.push(t("git: {when}", { when: r.scannedAt ? relTime(r.scannedAt) : t("dosud nečteno") }));
    if (r.inGithub) parts.push(t("GitHub: {when}", { when: r.syncedAt ? relTime(r.syncedAt) : t("dosud nečteno") }));
    return parts.join(" · ");
  }

  function portfolioRow(r) {
    const sub = r.description || (r.key.startsWith("local:") ? r.localPath : r.key);
    return h("tr", {},
      h("td", {}, h("a", { class: "repo-name", href: `#/repo/${r.id}`, title: freshness(r) }, r.name), h("span", { class: "sub", title: sub }, sub)),
      h("td", { class: "hide-sm" }, sourceBadges(r)),
      h("td", { html: C.sparkline(r.spark, "var(--series-1)"), title: I.plural(sum(r.spark), ["{n} commit za 26 týdnů", "{n} commity za 26 týdnů", "{n} commitů za 26 týdnů"]) }),
      h("td", { title: fmtDate(r.lastCommit) }, relTime(r.lastCommit)),
      h("td", { class: "num" }, fmt(r.commits)),
      h("td", { class: "num hide-sm" }, r.openIssues == null ? "–" : `${fmt(r.openIssues)} / ${fmt(r.openPrs)}`),
      h("td", { class: "hide-sm" }, r.inGithub ? ciStatus(r.ci) : "–"),
      h("td", {}, h("span", { class: "meter", title: t("{pct} % commitů s agentem", { pct: Math.round(r.agentShare * 100) }) }, h("span", { style: `width:${Math.round(r.agentShare * 100)}%` })),
        `${Math.round(r.agentShare * 100)} %`, r.sessions ? h("span", { class: "sub" }, t("{n} sessions", { n: fmt(r.sessions) })) : null),
      h("td", { class: "num hide-sm", title: r.tokens ? t("+ {cache} z cache", { cache: C.compact(r.tokens - r.freshTokens) }) : null }, r.freshTokens ? C.compact(r.freshTokens) : "–"));
  }

  // ---- mobile navigation ------------------------------------------------------------
  const menuBtn = document.getElementById("menu");
  const nav = document.querySelector(".nav");
  menuBtn.addEventListener("click", () => {
    const open = nav.classList.toggle("open");
    menuBtn.setAttribute("aria-expanded", String(open));
  });
  window.addEventListener("hashchange", () => { nav.classList.remove("open"); menuBtn.setAttribute("aria-expanded", "false"); });

  // Shared API for the page modules loaded after this file.
  window.RL = { api, h, t, I, localName, fmt, usd, fmtDate, fmtDateTime, relTime, hours, duration, tile, section, chartCard, segmented, dataTable, empty, sourceBadges, ciStatus,
    bucketKey, bucketRange, autoUnit, UNIT_LABEL, TYPE_GROUPS, typeGroupOf, pages, sum, route, refreshLastRun, BUNDLE,
    info, tabs, tabOf, PERIODS, daysOf, daysQuery, periodControl, delta, navigate, send, problems, csvButton, freshness, AGENT_SHARE_HELP };
  refreshLastRun();
  document.addEventListener("DOMContentLoaded", route);
  if (document.readyState !== "loading") setTimeout(route, 0);
})();
