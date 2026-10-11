// Agents page and the per-repository agent section.
(function () {
  "use strict";
  const C = window.Charts;
  const { api, h, t, I, localName, fmt, usd, fmtDate, fmtDateTime, relTime, duration, tile, section, chartCard, dataTable, empty,
    bucketKey, bucketRange, autoUnit } = window.RL;
  const TOOL_COLORS = { claude: "var(--series-2)", codex: "var(--series-1)" };
  const TOOL_LABEL = { claude: "Claude Code", codex: "Codex" };
  // Czech source labels, translated where shown.
  const KIND_LABEL = { agent: "agent", skill: "skill", command: "command", rule: "pravidlo", hook: "hook", permission: "oprávnění",
    mcp: "MCP server", instructions: "instrukce", settings: "nastavení", plugin: "plugin" };
  const SCOPE_LABEL = { project: "projekt", global: "globál", plugin: "plugin" };
  const kindLabel = (k) => (KIND_LABEL[k] ? t(KIND_LABEL[k]) : k);
  const scopeLabel = (s) => (SCOPE_LABEL[s] ? t(SCOPE_LABEL[s]) : s);
  const TOP_ROWS = 15;
  const MS_PER_HOUR = 3600000;

  const hoursFmt = (ms) => C.nf1(ms / MS_PER_HOUR, 1) + " h";

  function costNote(tot) {
    const unpriced = tot.unpricedTokens ? " · " + t("bez ceny {n} tokenů (chybí ceník)", { n: C.compact(tot.unpricedTokens) }) : "";
    if (!tot.costUsd) return t("ceník pro použité modely není k dispozici") + unpriced;
    const share = Math.round((tot.costReported / tot.costUsd) * 100);
    return t("odhad, neúplný · {pct} % hlásí sám Claude Code, zbytek z ceníku", { pct: share }) + unpriced;
  }

  const METRIC = {
    sessions: (d) => d.sessions,
    active: (d) => d.activeMs / MS_PER_HOUR,
    tokens: (d) => d.fresh,
    cost: (d) => d.costUsd,
  };
  const ROUNDED = { active: 10, cost: 100 };

  function timeCard(av, metric, title, note) {
    const days = [...new Set(av.daily.map((d) => d.day))].sort();
    const tools = [...new Set(av.daily.map((d) => d.tool))];
    return chartCard({
      title, note, span: 6,
      draw: (el) => {
        if (!days.length) return el.replaceChildren(empty(t("Žádné sessions.")));
        const unit = autoUnit(days[0], days[days.length - 1]);
        const keys = bucketRange(days[0], days[days.length - 1], unit);
        const at = new Map(keys.map((k, i) => [k, i]));
        const series = tools.map((tool) => ({ label: TOOL_LABEL[tool] || tool, color: TOOL_COLORS[tool] || "var(--series-3)", values: new Array(keys.length).fill(0) }));
        av.daily.forEach((d) => {
          series[tools.indexOf(d.tool)].values[at.get(bucketKey(d.day, unit))] += METRIC[metric](d);
        });
        const k = ROUNDED[metric];
        if (k) series.forEach((s) => (s.values = s.values.map((v) => Math.round(v * k) / k)));
        C.stackedBars(el, { labels: keys, series: series.filter((s) => s.values.some((v) => v > 0)), height: 220 });
      },
      table: { head: [t("Den"), t("Agent"), "Sessions", t("Aktivní čas"), t("Tokeny"), t("Cena")], rows: av.daily.map((d) => [d.day, TOOL_LABEL[d.tool] || d.tool, d.sessions, hoursFmt(d.activeMs), C.compact(d.tokens), d.costUsd ? usd(d.costUsd) : "–"]), num: [2, 3, 4, 5] },
    });
  }

  const COST_SOURCE = { reported: "hlásí agent", estimate: "z ceníku", mixed: "částečně" };

  const TREND_KINDS = [["skill", "Skills"], ["subagent", "Subagenti"], ["tool", "Nástroje"], ["mcp", "MCP"], ["slash", "Slash commandy"]];
  const SERIES = ["var(--series-1)", "var(--series-2)", "var(--series-3)", "var(--series-4)", "var(--series-5)", "var(--series-6)", "var(--series-7)"];

  // Weekly use of the most used skills, subagents, tools, MCP servers and slash commands.
  function trendCard(av) {
    const kinds = TREND_KINDS.filter(([k]) => av.trends && av.trends[k]);
    if (!kinds.length) return null;
    let kind = kinds[0][0];
    const card = chartCard({
      title: t("Použití v čase"), note: t("Týdně podle začátku session; nejčastějších šest jmen, zbytek jako „ostatní“."), span: 12,
      tools: window.RL.segmented(kinds.map(([k, l]) => [k, t(l)]), kind, (k) => { kind = k; card.rerender(); }),
      draw: (el) => {
        // Weeks without use are missing from the data; the axis must still be continuous.
        const tr = av.trends[kind];
        const weeks = window.RL.bucketRange(tr.weeks[0], tr.weeks[tr.weeks.length - 1], "week");
        const at = new Map(tr.weeks.map((w, i) => [w, i]));
        C.stackedBars(el, { labels: weeks, height: 240,
          series: tr.names.map((n, i) => ({ label: localName(n), color: SERIES[i] || "var(--series-8)", values: weeks.map((w) => (at.has(w) ? tr.counts[i][at.get(w)] : 0)) })) });
      },
      table: {
        get head() { return [t("Jméno"), ...av.trends[kind].weeks]; },
        get rows() { return av.trends[kind].names.map((n, i) => [localName(n), ...av.trends[kind].counts[i]]); },
        get num() { return av.trends[kind].weeks.map((_, i) => i + 1); },
      },
    });
    return card;
  }

  // Session length: active time, not the span between the first and the last record.
  function durationCard(av) {
    const d = av.durations;
    return chartCard({
      title: t("Délka sessions"),
      note: t("Aktivní čas session · medián {median}, 90. percentil {p90}.", { median: duration(d.medianMs), p90: duration(d.p90Ms) }), span: 6,
      draw: (el) => (d.sessions ? C.hBars(el, d.buckets.map((b) => ({ label: b.label, value: b.sessions })), { color: "var(--series-4)", format: fmt })
        : el.replaceChildren(empty(t("Žádné sessions.")))),
      table: { head: [t("Délka"), "Sessions"], rows: d.buckets.map((b) => [b.label, b.sessions]), num: [1] },
    });
  }

  // When configuration was created and last changed, from the git history of project files.
  function configTimelineCard(av) {
    const tl = av.configTimeline || [];
    const months = [...new Set(tl.map((m) => m.month))].sort();
    const kinds = [...new Set(tl.map((m) => m.kind))].sort();
    return chartCard({
      title: t("Vznik konfigurace v čase"), note: t("Kolik agentů, skills, pravidel… v repozitářích vzniklo v daném měsíci (první commit souboru). Globální konfigurace historii nemá."), span: 12,
      draw: (el) => {
        if (!months.length) return el.replaceChildren(empty(t("Žádná konfigurace s historií v gitu.")));
        const at = new Map(months.map((m, i) => [m, i]));
        const series = kinds.slice(0, SERIES.length).map((k, i) => ({ label: kindLabel(k), color: SERIES[i], values: new Array(months.length).fill(0) }));
        tl.forEach((m) => { const i = kinds.indexOf(m.kind); if (i < SERIES.length) series[i].values[at.get(m.month)] += m.created; });
        C.stackedBars(el, { labels: months, series, height: 220 });
      },
      table: { head: [t("Měsíc"), t("Druh"), t("Vzniklo"), t("Poslední změna")], rows: tl.map((m) => [m.month, kindLabel(m.kind), m.created, m.changed]), num: [2, 3] },
    });
  }

  function modelsCard(av) {
    const costCell = (m) => {
      if (m.costUsd == null) return h("span", { class: "estimate", title: t("Ceník modelu není známý; doplň ho na stránce Nastavení.") }, t("bez ceníku"));
      const label = COST_SOURCE[m.costSource] ? t(COST_SOURCE[m.costSource]) : "";
      return h("span", {}, usd(m.costUsd), " ", h("span", { class: "estimate" }, t("odhad · {source}", { source: label })));
    };
    return chartCard({
      title: t("Modely a tokeny"), note: t("Vstup bez cache, výstup, čtení a zápis cache. Ceny jsou vždy odhad."), span: 12,
      draw: (el) => el.replaceChildren(dataTable([t("Model"), "Sessions", t("Požadavky"), t("Vstup"), t("Výstup"), t("Cache čtení"), t("Cache zápis"), "Reasoning", t("Cena")],
        av.models.map((m) => [m.model, m.sessions, m.requests, C.compact(m.input), C.compact(m.output), C.compact(m.cacheRead), C.compact(m.cacheWrite), C.compact(m.reasoning), costCell(m)]), [1, 2, 3, 4, 5, 6, 7])),
    });
  }

  function counterCard(av, kind, title, note, opts) {
    const o = opts || {};
    const rows = (av.counters[kind] || []).slice(0, o.top || TOP_ROWS);
    const value = o.value || ((c) => c.count);
    return chartCard({
      title, note, span: o.span || 6,
      draw: (el) => (rows.length ? C.hBars(el, rows.map((c) => ({ label: localName(c.name), value: value(c), note: o.note ? o.note(c) : t("{n} sessions", { n: fmt(c.sessions) }) })), { color: o.color || "var(--series-1)", format: o.format || fmt }) : el.replaceChildren(empty(t("Nic.")))),
      table: { head: o.head || [t("Jméno"), t("Počet"), t("Chyby"), "Sessions"], rows: (av.counters[kind] || []).map((c) => (o.row ? o.row(c) : [localName(c.name), c.count, c.errors, c.sessions])), num: [1, 2, 3] },
    });
  }

  // Configured agents/skills/commands with usage; unused ones are struck through.
  function usageBlock(av, scopeRepo) {
    const usable = av.config.filter((c) => c.usable && c.present);
    const used = usable.filter((c) => c.uses > 0).sort((a, b) => b.uses - a.uses);
    const unused = usable.filter((c) => c.uses === 0);
    const byKind = (list, kind) => list.filter((c) => c.kind === kind);
    const chips = (list) => h("div", { class: "chips" }, list.map((c) => h("span", { class: "chip" + (c.uses ? "" : " unused"), title: `${scopeLabel(c.scope)}${c.repo ? " · " + c.repo : ""} · ${c.path}` }, localName(c.name), c.uses ? ` ×${c.uses}` : "")));
    const unusedKinds = ["skill", "agent", "command"].map((k) => {
      const list = byKind(unused, k);
      const scopes = {};
      list.forEach((c) => (scopes[c.scope] = (scopes[c.scope] || 0) + 1));
      const where = Object.entries(scopes).map(([s, n]) => `${scopeLabel(s)} ${n}`).join(", ") || "–";
      return h("div", { style: "margin-top:10px" }, h("p", { class: "note" }, t("{kind}: {n} nespuštěno ({where})", { kind: kindLabel(k), n: list.length, where })),
        list.length ? h("details", {}, h("summary", { class: "note" }, t("zobrazit")), chips(list.slice(0, 400))) : null);
    });
    const unconf = Object.entries(av.unconfigured || {}).flatMap(([kind, rows]) => rows.map((r) => ({ kind, ...r })));
    return h("div", { class: "grid" },
      h("section", { class: "card span-6" }, h("h3", {}, t("Reálně použité")),
        h("p", { class: "note" }, scopeRepo ? t("{used} z {total} nakonfigurovaných agentů, skills a commandů (v tomto repu)", { used: used.length, total: usable.length })
          : t("{used} z {total} nakonfigurovaných agentů, skills a commandů", { used: used.length, total: usable.length })),
        used.length ? chips(used) : empty(t("Žádné použití.")),
        unconf.length ? h("div", { style: "margin-top:14px" }, h("p", { class: "note" }, t("Použité bez nalezené konfigurace (vestavěné, nebo z dnes smazaného zdroje):")),
          h("div", { class: "chips" }, unconf.map((r) => h("span", { class: "chip", title: r.kind }, `${r.kind === "slash" ? "/" : ""}${localName(r.name)} ×${r.count}`)))) : null),
      h("section", { class: "card span-6" }, h("h3", {}, t("Jen nakonfigurované, nikdy nespuštěné")),
        h("p", { class: "note" }, t("Počítá se ze sessions, které jsou v transcriptech (Claude Code mění starší soubory po čase).")), unusedKinds));
  }

  const CONFIG_LIST_MAX = 300;

  function configSummary(av) {
    const scopes = ["project", "global", "plugin"];
    const kinds = [...new Set(av.config.map((c) => c.kind))].sort();
    const count = (k, s) => av.config.filter((c) => c.kind === k && c.scope === s && c.present).length;
    return dataTable([t("Druh"), ...scopes.map(scopeLabel), t("Smazané (jen v historii)")],
      kinds.map((k) => [kindLabel(k), ...scopes.map((s) => count(k, s)), av.config.filter((c) => c.kind === k && !c.present).length]), [1, 2, 3, 4]);
  }

  function configTable(av) {
    let kind = "all";
    const kinds = ["all", ...[...new Set(av.config.map((c) => c.kind))].sort()];
    const card = chartCard({
      title: t("Inventář konfigurace"), note: t("Co je nakonfigurované, odkud, kdy to vzniklo a kolikrát se to měnilo (git historie souborů v repu). Hodnoty proměnných a hesla v pravidlech jsou maskované."),
      tools: h("select", { "aria-label": t("Druh"), onchange: (e) => { kind = e.target.value; card.rerender(); } },
        kinds.map((k) => h("option", { value: k }, k === "all" ? t("souhrn") : kindLabel(k)))),
      draw: (el) => {
        if (kind === "all") return el.replaceChildren(configSummary(av));
        const list = av.config.filter((c) => c.kind === kind);
        el.replaceChildren(
          list.length > CONFIG_LIST_MAX ? h("p", { class: "note" }, t("Zobrazeno prvních {n} z {total}.", { n: CONFIG_LIST_MAX, total: list.length })) : null,
          dataTable([t("Rozsah"), t("Nástroj"), t("Jméno"), t("Detail"), t("Vzniklo"), t("Změny"), t("Použití")],
            list.slice(0, CONFIG_LIST_MAX).map((c) => [scopeLabel(c.scope) + (c.repo ? ` · ${c.repo}` : ""), c.tool,
              h("span", { title: c.path, class: c.present ? "" : "estimate" }, c.name + (c.present ? "" : " " + t("(smazáno)"))), c.detail,
              c.first ? fmtDate(c.first) : "–", c.changes || "–", c.usable ? c.uses : "–"]), [5, 6]));
      },
    });
    return card;
  }

  function hooksBlock(av) {
    const den = av.counters.denial || [];
    return h("div", { class: "grid" },
      counterCard(av, "hook", t("Hooky podle události"), t("Kolikrát hook běžel; chyby = zablokováno, zrušeno nebo selhalo."), { color: "var(--series-6)", note: (c) => I.plural(c.errors, ["{n} chyba", "{n} chyby", "{n} chyb"]), head: [t("Událost"), t("Běhy"), t("Chyby"), "Sessions"] }),
      h("section", { class: "card span-6" }, h("h3", {}, t("Zásahy hooků a zamítnutí")),
        h("p", { class: "note" }, t("Zamítnuté nástroje (uživatel, auto mode, pravidla) a zásahy hooků.")),
        den.length ? dataTable([t("Zamítnutí"), t("Počet")], den.map((c) => [c.name, c.count]), [1]) : empty(t("Žádná zamítnutí.")),
        h("div", { style: "margin-top:12px" }),
        (av.counters.hook_block || []).length ? dataTable([t("Zásah hooku"), t("Počet")], av.counters.hook_block.map((c) => [c.name, c.count]), [1]) : empty(t("Žádné zásahy hooků."))));
  }

  function attributionCard(av) {
    return chartCard({
      title: t("Commity s účastí agenta"), note: t("Trailer Co-Authored-By/Agent vs. commit uvnitř okna aktivity session ve stejném repu (±10 min)."), span: 12,
      draw: (el) => {
        if (!av.attribution.length) return el.replaceChildren(empty(t("Žádné commity.")));
        const pct = (n, d) => (d ? Math.round((n / d) * 100) : 0);
        C.lineChart(el, { labels: av.attribution.map((m) => m.month), series: [
          { label: t("% s trailerem"), color: "var(--series-1)", values: av.attribution.map((m) => pct(m.trailer, m.commits)) },
          { label: t("% v session"), color: "var(--series-2)", values: av.attribution.map((m) => pct(m.inSession, m.commits)) },
          { label: t("% celkem (kterýkoli signál)"), color: "var(--series-3)", values: av.attribution.map((m) => pct(m.either, m.commits)) },
        ] });
      },
      table: { head: [t("Měsíc"), t("Commity"), "Trailer", t("V session"), t("Kterýkoli")], rows: av.attribution.map((m) => [m.month, m.commits, m.trailer, m.inSession, m.either]), num: [1, 2, 3, 4] },
    });
  }

  function projectsTable(av) {
    return dataTable([t("Repozitář"), "Sessions", t("Aktivní čas"), t("Tokeny"), t("Cena (odhad)"), t("Hlavní model"), t("Konfigurace"), t("S agentem · 90 dní"), t("Poslední session")],
      av.projects.map((p) => [h("a", { href: `#/repo/${p.repoId}`, class: "repo-name" }, p.name), p.sessions, hoursFmt(p.activeMs), C.compact(p.tokens), p.costUsd ? usd(p.costUsd) : "–",
        p.topModel || "–", p.configured, `${Math.round(p.agentShare * 100)} %`, p.lastSession ? relTime(p.lastSession) : "–"]), [1, 2, 3, 4, 6, 7]);
  }

  // Tabs of the agent views; the repository page shows the first three.
  const AGENT_TABS = [["overview", "Přehled"], ["usage", "Používání"], ["config", "Konfigurace"], ["sessions", "Sessions"]];

  function overviewTab(av, mode) {
    const tot = av.totals;
    const out = [];
    if (mode === "repo") {
      out.push(h("div", { class: "tiles" }, tile("Sessions", fmt(tot.sessions), t("{n} ve worktree", { n: tot.inWorktree })), tile(t("Aktivní čas"), hoursFmt(tot.activeMs)),
        tile(t("Tokeny"), C.compact(tot.fresh), t("+ {cache} z cache", { cache: C.compact(tot.tokens - tot.fresh) })), tile(t("Cena"), usd(tot.costUsd), t("odhad"))));
    }
    out.push(section(1, t("Sessions v čase")),
      h("div", { class: "grid" }, timeCard(av, "sessions", t("Počet sessions"), t("Podle začátku session.")), timeCard(av, "active", t("Aktivní hodiny"), t("Součet délek tahů (turn_duration), jinak aktivita bez pauz > 5 min.")),
        timeCard(av, "tokens", t("Tokeny v čase"), t("Vstup + výstup bez cache, včetně vláken, která session spustila.")),
        timeCard(av, "cost", t("Cena v čase (USD, odhad)"), t("Jen známá cena: co hlásí agent, nebo podle ceníku. Tokeny bez ceny se nezapočítají.")),
        durationCard(av)),
      section(2, t("Modely")), h("div", { class: "grid" }, modelsCard(av)));
    return out;
  }

  function usageTab(av, mode) {
    return [section(1, t("Nástroje, subagenti, skills")),
      h("div", { class: "grid" },
        counterCard(av, "tool", t("Nejpoužívanější nástroje"), t("Volání podle jména; chyby = tool_result s is_error."), { note: (c) => I.plural(c.errors, ["{n} chyba", "{n} chyby", "{n} chyb"]) + " · " + t("{n} sessions", { n: fmt(c.sessions) }) }),
        counterCard(av, "subagent", t("Subagenti"), t("Typ subagenta, počet spuštění a celková délka (ze souborů subagentů)."), { color: "var(--series-7)", note: (c) => (c.durationMs ? t("celkem {d}", { d: duration(c.durationMs) }) : t("bez délky")),
          head: [t("Typ"), t("Spuštění"), t("Celková délka"), "Sessions"], row: (c) => [localName(c.name), c.count, c.durationMs ? duration(c.durationMs) : "–", c.sessions] }),
        counterCard(av, "skill", t("Skills (nástroj Skill)"), null, { color: "var(--series-3)" }),
        counterCard(av, "slash", t("Slash commandy"), null, { color: "var(--series-4)" }),
        counterCard(av, "tool_error", t("Chyby nástrojů"), null, { color: "var(--series-8)" }),
        counterCard(av, "mcp", t("MCP servery"), t("Volání nástrojů mcp__server__*."), { color: "var(--series-5)" })),
      h("div", { class: "grid", style: "margin-top:16px" }, trendCard(av)),
      section(2, t("Nakonfigurováno vs. použito")), usageBlock(av, mode === "repo"),
      section(3, t("Hooky a zamítnutí")), hooksBlock(av)];
  }

  function configTab(av) {
    return [section(1, t("Konfigurace")), h("div", { class: "grid" }, configTable(av), configTimelineCard(av)),
      section(2, t("Commity a agenti")), h("div", { class: "grid" }, attributionCard(av))];
  }

  // agentTabs renders the tab bar and the chosen tab; param is the URL key of the tab.
  function agentTabs(av, mode, params, param) {
    if (!av || (!av.totals.sessions && !av.config.length)) {
      return [empty(mode === "repo" ? t("V tomto repu nejsou žádné sessions ani konfigurace agentů.") : t("Zatím nejsou sebraná žádná data o agentech."))];
    }
    const defs = (mode === "repo" ? AGENT_TABS.slice(0, 3) : AGENT_TABS).map(([k, l]) => [k, t(l)]);
    const current = defs.some(([k]) => k === params.get(param)) ? params.get(param) : defs[0][0];
    const bar = tabsFor(defs, current, params, param);
    const body = { overview: () => overviewTab(av, mode), usage: () => usageTab(av, mode), config: () => configTab(av) }[current];
    return [bar, ...(body ? body() : [])];
  }

  // tabsFor is RL.tabs with a custom URL key (the repository page nests agent tabs in its own).
  function tabsFor(defs, current, params, param) {
    if (param === "tab") return window.RL.tabs(defs, current, params);
    const nav = h("div", { class: "tabs sub" });
    for (const [key, label] of defs) {
      nav.appendChild(h("button", { type: "button", "aria-pressed": String(key === current), onclick: () => {
        const p = new URLSearchParams(params);
        if (key === defs[0][0]) p.delete(param); else p.set(param, key);
        window.RL.navigate(p);
      } }, label));
    }
    return nav;
  }

  function projectsTable(av) {
    return dataTable([t("Repozitář"), "Sessions", t("Aktivní čas"), t("Tokeny vč. cache"), t("Cena (odhad)"), t("Hlavní model"), t("Konfigurace"), t("S agentem · 90 dní"), t("Poslední session")],
      av.projects.map((p) => [h("a", { href: `#/repo/${p.repoId}`, class: "repo-name" }, p.name), p.sessions, hoursFmt(p.activeMs), C.compact(p.tokens), p.costUsd ? usd(p.costUsd) : "–",
        p.topModel || "–", p.configured, `${Math.round(p.agentShare * 100)} %`, p.lastSession ? relTime(p.lastSession) : "–"]), [1, 2, 3, 4, 6, 7], t("Porovnání projektů"));
  }

  function sessionRows(rows) {
    return rows.map((s) => [
      h("a", { href: `#/session/${encodeURIComponent(s.id)}`, class: "repo-name nowrap", title: fmtDateTime(s.startedAt) }, relTime(s.startedAt)),
      TOOL_LABEL[s.tool] || s.tool,
      s.repoId ? h("a", { href: `#/repo/${s.repoId}` }, s.repo) : h("span", { class: "mono" }, s.cwd),
      hoursFmt(s.activeMs), s.turns, h("span", { title: t("+ {cache} z cache", { cache: C.compact(s.tokens - s.fresh) }) }, C.compact(s.fresh)),
      s.costUsd ? usd(s.costUsd) : "–", s.topModel || "–"]);
  }
  const SESSION_HEAD = () => [t("Začátek"), t("Agent"), t("Repozitář"), t("Aktivní čas"), t("Tahy"), t("Tokeny"), t("Cena"), t("Hlavní model")];
  const PAGE_SIZE = 50;

  // sessionList is the filterable, paged session list. The static export filters the sessions it
  // carries; the server filters all of them.
  function sessionList(av, days) {
    const f = { repo: "", tool: "", model: "", offset: 0 };
    const out = h("div", {});
    const pager = h("div", { class: "toolbar" });
    const select = (label, options, key) => {
      const el = h("select", { "aria-label": label }, h("option", { value: "" }, label), options.map(([v, l]) => h("option", { value: v }, l)));
      el.addEventListener("change", () => { f[key] = el.value; f.offset = 0; load(); });
      return el;
    };
    const filters = h("div", { class: "toolbar" },
      select(t("všechna repa"), av.projects.map((p) => [String(p.repoId), p.name]), "repo"),
      select(t("všichni agenti"), Object.entries(TOOL_LABEL), "tool"),
      select(t("všechny modely"), av.models.map((m) => [m.model, m.model]), "model"), pager);
    const local = () => {
      const rows = av.sessions.filter((s) => (!f.repo || String(s.repoId) === f.repo) && (!f.tool || s.tool === f.tool) && (!f.model || s.topModel === f.model));
      return Promise.resolve({ total: rows.length, rows: rows.slice(f.offset, f.offset + PAGE_SIZE) });
    };
    const remote = () => {
      const q = new URLSearchParams({ limit: PAGE_SIZE, offset: f.offset });
      if (days !== "all") q.set("days", days);
      ["repo", "tool", "model"].forEach((k) => f[k] && q.set(k, f[k]));
      return api("/api/sessions?" + q);
    };
    const load = async () => {
      const page = await (window.RL.BUNDLE ? local() : remote());
      const from = page.total ? f.offset + 1 : 0, to = Math.min(f.offset + PAGE_SIZE, page.total);
      pager.replaceChildren(h("span", { class: "count" }, t("{from}–{to} z {total}", { from: fmt(from), to: fmt(to), total: fmt(page.total) })),
        h("button", { type: "button", class: "btn", disabled: f.offset === 0, onclick: () => { f.offset = Math.max(0, f.offset - PAGE_SIZE); load(); } }, "← " + t("novější")),
        h("button", { type: "button", class: "btn", disabled: to >= page.total, onclick: () => { f.offset += PAGE_SIZE; load(); } }, t("starší") + " →"));
      out.replaceChildren(page.rows.length ? dataTable(SESSION_HEAD(), sessionRows(page.rows), [3, 4, 5, 6], "sessions") : empty(t("Žádné sessions.")));
    };
    load();
    return [filters, out];
  }

  // unmatchedCard lists sessions outside the repositories; on the server each directory can be
  // assigned to a repository, which is remembered for later collections.
  function unmatchedCard(av, repos) {
    const live = !window.RL.BUNDLE;
    const rows = av.unmatched.map((u) => {
      const cells = [h("span", { class: "mono" }, u.cwd), u.sessions];
      if (live) {
        const sel = h("select", { "aria-label": t("Repozitář") }, h("option", { value: "" }, t("přiřadit k…")), repos.map((r) => h("option", { value: r.id }, r.name)));
        const msg = h("span", { class: "field-hint" });
        const btn = h("button", { type: "button", class: "btn", onclick: async () => {
          if (!sel.value) return;
          btn.disabled = true;
          const r = await window.RL.send("POST", "/api/assign", { cwd: u.cwd, repoId: Number(sel.value) });
          if (r.ok) msg.replaceChildren(h("span", { class: "status good" }, t("Přiřazeno {n} sessions.", { n: r.data.sessions })));
          else { btn.disabled = false; msg.textContent = window.RL.problems(r).join(" "); }
        } }, t("Přiřadit"));
        cells.push(h("span", { class: "assign" }, sel, btn, msg));
      }
      return cells;
    });
    return h("section", { class: "card span-12" }, h("h3", {}, t("Sessions mimo vlastní repozitáře")),
      h("p", { class: "note" }, t("cwd nepatří žádnému vlastnímu repu (cizí repa, domovský adresář, přesunuté nebo smazané složky). Adresář lze přiřadit k repozitáři; přiřazení se uloží do Nastavení.")),
      rows.length ? dataTable(live ? [t("Adresář"), "Sessions", ""] : [t("Adresář"), "Sessions"], rows, [1]) : empty(t("Žádné.")));
  }

  async function sessionsTab(av, days) {
    const other = Object.entries(av.otherAgents || {});
    const repos = window.RL.BUNDLE ? [] : ((await api("/api/portfolio")) || []).sort((a, b) => a.name.localeCompare(b.name));
    return [section(1, t("Sessions")), ...sessionList(av, days),
      section(2, t("Porovnání projektů")), av.projects.length ? projectsTable(av) : empty(t("Žádné projekty se sessions.")),
      section(3, t("Kvalita dat")),
      h("div", { class: "grid" }, unmatchedCard(av, repos),
        h("section", { class: "card span-12" }, h("h3", {}, t("Další agenti — nalezeno, neparsováno")),
          h("p", { class: "note" }, t("Formát těchto souborů repolens zatím nečte; počet souborů ukazuje, že data existují.")),
          other.length ? dataTable([t("Agent"), t("Souborů")], other.map(([k, cnt]) => [k, cnt]), [1]) : empty(t("Nic dalšího nenalezeno."))))];
  }

  window.RL.agentTabs = (av, params) => agentTabs(av, "repo", params, "atab");

  // ---- one session -----------------------------------------------------------------------
  window.RL.pages.session = async (parts) => {
    const id = decodeURIComponent(parts[0] || "");
    const d = await api(`/api/session/${encodeURIComponent(id)}`);
    if (!d) return [h("h1", {}, t("Session nenalezena")), h("p", {}, h("a", { href: "#/agents" }, "← " + t("Agenti")))];
    const s = d.session;
    const counter = (kind, title) => (d.counters[kind] || []).length
      ? h("section", { class: "card span-6" }, h("h3", {}, title), dataTable([t("Jméno"), t("Počet"), t("Chyby")], d.counters[kind].map((c) => [localName(c.name), c.count, c.errors]), [1, 2]))
      : null;
    return [
      h("p", {}, h("a", { href: "#/agents" }, "← " + t("Agenti"))),
      h("div", { class: "kicker" }, (TOOL_LABEL[s.tool] || s.tool) + " · session"),
      h("h1", { class: "mono", style: "font-size:clamp(18px,3vw,28px)" }, s.id),
      h("p", { class: "lede" }, s.repoId ? h("a", { href: `#/repo/${s.repoId}` }, s.repo) : h("span", { class: "mono" }, s.cwd)),
      h("div", { class: "tiles", style: "margin-top:16px" },
        tile(t("Začátek"), fmtDateTime(s.startedAt), t("konec {when}", { when: fmtDateTime(s.endedAt) })),
        tile(t("Aktivní čas"), hoursFmt(s.activeMs), I.plural(d.windows, ["{n} okno aktivity", "{n} okna aktivity", "{n} oken aktivity"])),
        tile(t("Tahy"), fmt(s.turns)),
        tile(t("Tokeny"), C.compact(s.tokens), s.threads ? I.plural(s.threads, ["včetně {n} vlákna", "včetně {n} vláken", "včetně {n} vláken"]) : null),
        tile(t("Cena"), s.costUsd ? usd(s.costUsd) : "–", t("odhad"))),
      h("p", { class: "note", style: "margin-top:12px" }, t("Jen jména a počty; obsah promptů, odpovědí ani výstupů nástrojů se neukládá.")),
      section(1, t("Modely")),
      dataTable([t("Model"), t("Požadavky"), t("Vstup"), t("Výstup"), t("Cache čtení"), t("Cache zápis"), t("Cena")],
        d.models.map((m) => [m.model, m.requests, C.compact(m.input), C.compact(m.output), C.compact(m.cacheRead), C.compact(m.cacheWrite), m.costUsd == null ? "–" : usd(m.costUsd)]), [1, 2, 3, 4, 5]),
      section(2, t("Nástroje, subagenti, skills")),
      h("div", { class: "grid" }, counter("tool", t("Nástroje")), counter("subagent", t("Subagenti")), counter("skill", "Skills"),
        counter("slash", t("Slash commandy")), counter("mcp", t("MCP servery")), counter("denial", t("Zamítnutí")), counter("hook", t("Hooky podle události"))),
      ...(d.threads.length ? [section(3, t("Vlákna spuštěná session")), dataTable([t("Začátek"), "ID", t("Aktivní čas"), t("Tokeny")],
        d.threads.map((x) => [fmtDateTime(x.startedAt), h("span", { class: "mono" }, x.id), hoursFmt(x.activeMs), C.compact(x.tokens)]), [2, 3])] : []),
    ];
  };

  window.RL.pages.agents = async (_, params) => {
    const days = window.RL.daysOf(params);
    const av = await api("/api/agents" + window.RL.daysQuery(days));
    const tot = av.totals;
    const tab = window.RL.tabOf(AGENT_TABS, params);
    const priced = (av.pricesDate ? " · " + t("ceník k {date}", { date: av.pricesDate }) : "") +
      (av.priceListDate ? " · " + t("LiteLLM k {date}", { date: av.priceListDate }) : "");
    const head = [
      h("div", { class: "hero" },
        h("div", {}, h("h1", { class: "kicker" }, t("Kódovací agenti")), h("div", { class: "hero-figure" }, fmt(tot.sessions),
          h("small", {}, `sessions · ${tot.first ? fmtDate(tot.first) : "–"} – ${tot.last ? fmtDate(tot.last) : "–"}`))),
        h("div", { class: "tiles" },
          ...av.byTool.map((b) => tile(TOOL_LABEL[b.tool] || b.tool, t("{n} sessions", { n: fmt(b.sessions) }), t("{hours} · {tokens} tokenů", { hours: hoursFmt(b.activeMs), tokens: C.compact(b.fresh) }))),
          tile([t("Tokeny"), " ", window.RL.info(t("Vstup + výstup. Čtení a zápis cache se počítají zvlášť: opakují stejný kontext a v součtu by přehlušily skutečnou práci."))],
            C.compact(tot.fresh), t("+ {cache} z cache · {turns}", { cache: C.compact(tot.tokens - tot.fresh), turns: I.plural(tot.turns, ["{n} tah", "{n} tahy", "{n} tahů"], { n: fmt(tot.turns) }) })),
          tile(t("Cena"), usd(tot.costUsd), costNote(tot) + priced))),
      h("p", { class: "lede", style: "margin-top:12px" }, t("Z transcriptů se čtou jen jména a počty (nástroj, skill, agent, model, tokeny, časy). Obsah promptů ani výstupů se neukládá.")),
      window.RL.periodControl(params, t("Sessions, tokeny, modely a použití za zvolené období; projekty, commity a konfigurace za celou dobu.")),
    ];
    if (tab === "sessions") return [...head, window.RL.tabs(AGENT_TABS.map(([k, l]) => [k, t(l)]), tab, params), ...(await sessionsTab(av, days))];
    return [...head, ...agentTabs(av, "all", params, "tab")];
  };
})();
