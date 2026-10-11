// Repository detail page.
(function () {
  "use strict";
  const C = window.Charts;
  const { api, h, t, I, fmt, fmtDate, fmtDateTime, relTime, hours, tile, section, chartCard, segmented, dataTable, empty, sourceBadges, ciStatus,
    bucketKey, bucketRange, autoUnit, TYPE_GROUPS, typeGroupOf, sum } = window.RL;
  const TOP_AUTHORS = 15;

  // Groups daily rows into buckets; returns {unit, keys, at(key)->index}.
  function buckets(days, unitOverride) {
    if (!days.length) return null;
    const first = days[0], last = days[days.length - 1];
    const unit = unitOverride === "auto" || !unitOverride ? autoUnit(first, last) : unitOverride;
    const keys = bucketRange(first, last, unit);
    const index = new Map(keys.map((k, i) => [k, i]));
    return { unit, keys, at: (day) => index.get(bucketKey(day, unit)) };
  }

  function commitsCard(d) {
    let unit = "auto";
    const days = [...new Set(d.dailyTypes.map((x) => x.day))].sort();
    const draw = (el) => {
      const b = buckets(days, unit);
      if (!b) return el.replaceChildren(empty(t("Žádné commity.")));
      const series = TYPE_GROUPS.map((g) => ({ label: t(g.label), color: g.color, values: new Array(b.keys.length).fill(0) }));
      const idx = Object.fromEntries(TYPE_GROUPS.map((g, i) => [g.key, i]));
      for (const x of d.dailyTypes) series[idx[typeGroupOf[x.type] || "other"]].values[b.at(x.day)] += x.count;
      C.stackedBars(el, { labels: b.keys, series: series.filter((s) => sum(s.values) > 0), height: 240 });
    };
    const tools = segmented([["auto", "auto"], ["day", t("dny")], ["week", t("týdny")], ["month", t("měsíce")]], unit, (u) => { unit = u; card.rerender(); });
    const byType = {};
    d.dailyTypes.forEach((x) => (byType[x.day] = byType[x.day] || {}, byType[x.day][x.type] = (byType[x.day][x.type] || 0) + x.count));
    const card = chartCard({
      title: t("Commity podle typu"), note: t("Conventional commits, seskupeno automaticky podle rozpětí historie.") + " " + sourceNote(d),
      draw, tools,
      table: { head: [t("Den"), t("Typy"), t("Celkem")], rows: days.map((day) => [day, Object.entries(byType[day]).map(([ty, n]) => `${ty} ${n}`).join(", "), sum(Object.values(byType[day]))]), num: [2] },
    });
    return card;
  }

  // sourceNote names where the commit data of this repository comes from.
  function sourceNote(d) {
    return d.repo.inLocal ? t("Zdroj: lokální git log.") : t("Zdroj: GitHub API (historie výchozí větve, repo není naklonované).");
  }

  function cumulativeCard(d) {
    const perDay = {};
    d.dailyTypes.forEach((x) => (perDay[x.day] = (perDay[x.day] || 0) + x.count));
    const days = Object.keys(perDay).sort();
    return chartCard({
      title: t("Kumulativně"), note: t("Celkový počet commitů v čase."), span: 6,
      draw: (el) => {
        const b = buckets(days);
        if (!b) return el.replaceChildren(empty(t("Žádné commity.")));
        const vals = new Array(b.keys.length).fill(0);
        days.forEach((day) => (vals[b.at(day)] += perDay[day]));
        let acc = 0;
        C.lineChart(el, { labels: b.keys, series: [{ label: t("commity celkem"), color: "var(--series-1)", values: vals.map((v) => (acc += v)) }], area: true });
      },
      table: { head: [t("Den"), t("Commity")], rows: days.map((x) => [x, perDay[x]]), num: [1] },
    });
  }

  function linesCard(d) {
    const days = d.dailyLines.map((x) => x.day);
    return chartCard({
      title: t("Přidané a smazané řádky"), note: t("Bez merge commitů; binární soubory se nepočítají.") + " " + sourceNote(d), span: 6,
      draw: (el) => {
        const b = buckets(days);
        if (!b) return el.replaceChildren(empty(t("Žádná data.")));
        const add = new Array(b.keys.length).fill(0), del = new Array(b.keys.length).fill(0);
        d.dailyLines.forEach((x) => { add[b.at(x.day)] += x.add; del[b.at(x.day)] += x.del; });
        C.divergingBars(el, { labels: b.keys, up: { label: t("přidáno"), color: "var(--series-1)", values: add }, down: { label: t("smazáno"), color: "var(--series-8)", values: del } });
      },
      table: { head: [t("Den"), t("Přidáno"), t("Smazáno")], rows: d.dailyLines.map((x) => [x.day, x.add, x.del]), num: [1, 2] },
    });
  }

  function issuesCard(d) {
    if (!d.repo.inGithub) return h("section", { class: "card span-6" }, h("h3", {}, t("Issues a PR")), empty(t("GitHub data nejsou k dispozici (repo bez remote, bez tokenu, nebo collect --no-github).")));
    const weeks = d.issues.map((x) => x.week);
    return chartCard({
      title: t("Issues založené vs. uzavřené"),
      note: t("Medián uzavření issue {issue}, merge PR {pr}. Zdroj: GitHub API.", { issue: hours(d.issueTimes.medianIssueCloseHours), pr: hours(d.issueTimes.medianPrMergeHours) }), span: 6,
      draw: (el) => {
        if (!weeks.length) return el.replaceChildren(empty(t("Žádné issues ani PR.")));
        const keys = bucketRange(weeks[0], weeks[weeks.length - 1], "week");
        const at = new Map(keys.map((k, i) => [k, i]));
        const mk = () => new Array(keys.length).fill(0);
        const s = { io: mk(), ic: mk(), po: mk(), pm: mk() };
        d.issues.forEach((x) => { const i = at.get(x.week); s.io[i] += x.issuesOpened; s.ic[i] += x.issuesClosed; s.po[i] += x.prsOpened; s.pm[i] += x.prsMerged; });
        C.lineChart(el, { labels: keys, series: [
          { label: t("issues založené"), color: "var(--series-1)", values: s.io },
          { label: t("issues uzavřené"), color: "var(--series-2)", values: s.ic },
          { label: t("PR založené"), color: "var(--series-3)", values: s.po },
          { label: t("PR mergnuté"), color: "var(--series-4)", values: s.pm },
        ] });
      },
      table: { head: [t("Týden"), "Issues +", "Issues −", "PR +", "PR merge"], rows: d.issues.map((x) => [x.week, x.issuesOpened, x.issuesClosed, x.prsOpened, x.prsMerged]), num: [1, 2, 3, 4] },
    });
  }

  function authorsCard(d) {
    const rows = d.authors.slice(0, TOP_AUTHORS);
    return chartCard({
      title: t("Autoři"), note: t("Sloučené identity (stejný člověk pod více jmény či e-maily). Ruční mapa v Nastavení."), span: 6,
      draw: (el) => C.hBars(el, rows.map((a) => ({ label: a.name + (a.bot ? " (bot)" : ""), value: a.commits, note: a.emails.join(", ") })), { format: fmt }),
      table: { head: [t("Autor"), t("E-maily"), t("Commity"), "+", "−", t("První"), t("Poslední")], rows: d.authors.map((a) => [a.name, a.emails.join(", "), a.commits, a.add, a.del, fmtDate(a.first), fmtDate(a.last)]), num: [2, 3, 4] },
    });
  }

  function dirsCard(d) {
    return chartCard({
      title: t("Nejaktivnější adresáře"), note: t("Počet commitů, které adresář změnily."), span: 6,
      draw: (el) => (d.dirs.length ? C.hBars(el, d.dirs.map((x) => ({ label: x.dir, value: x.commits, note: `+${fmt(x.add)} / −${fmt(x.del)}` })), { color: "var(--series-3)", format: fmt })
        : el.replaceChildren(empty(d.repo.inLocal ? t("Žádná data.") : t("Rozpad po adresářích je jen z lokálního klonu; toto repo je jen na GitHubu.")))),
      table: { head: [t("Adresář"), t("Commity"), "+", "−"], rows: d.dirs.map((x) => [x.dir, x.commits, x.add, x.del]), num: [1, 2, 3] },
    });
  }

  function recentTable(d) {
    return dataTable([t("Kdy"), t("Autor"), t("Zpráva"), t("Typ"), "+", "−", t("Agent"), t("Zdroj")],
      d.recent.map((c) => [h("span", { title: fmtDateTime(c.time), class: "nowrap" }, relTime(c.time)), h("span", { style: "white-space:nowrap" }, c.author),
        h("span", { title: c.hash }, window.RL.localName(c.subject)), c.type, c.add, c.del, c.agents || (c.inSession ? t("v session") : "–"),
        c.source === "github-api" ? "GitHub" : "git"]), [4, 5]);
  }

  function openItems(d) {
    if (!d.open.length) return empty(t("Žádné otevřené issues ani PR."));
    const now = Date.now() / 1000;
    return dataTable(["#", t("Název"), t("Druh"), t("Autor"), t("Založeno"), t("Štítky")],
      d.open.map((o) => [o.number, window.RL.localName(o.title), o.isPr ? "PR" : "issue", o.author || "–",
        h("span", { class: (now - o.created) / 86400 > STALE_DAYS ? "stale" : null, title: fmtDate(o.created) }, relTime(o.created)), o.labels || "–"]),
      [0], t("Otevřené issues a PR"));
  }
  // STALE_DAYS marks open items as old, as the attention panel does for pull requests.
  const STALE_DAYS = 14;

  function githubTab(d) {
    if (!d.repo.inGithub) return [empty(t("GitHub data nejsou k dispozici (repo bez remote, bez tokenu, nebo collect --no-github)."))];
    return [
      section(1, t("Otevřené issues a PR")),
      h("p", { class: "note" }, t("Aktuální stav, nejstarší nahoře; starší než {n} dní jsou zvýrazněné.", { n: STALE_DAYS })),
      openItems(d),
      section(2, "GitHub"),
      h("div", { class: "grid" },
        h("section", { class: "card span-6" }, h("h3", {}, t("Milníky")),
          d.milestones.length ? dataTable([t("Milník"), t("Stav"), t("Termín"), t("Otevřené"), t("Hotové")], d.milestones.map((m) => [m.title, m.state === "OPEN" ? t("otevřený") : t("uzavřený"), m.dueOn ? fmtDate(m.dueOn) : "–", m.open, m.closed]), [3, 4]) : empty(t("Žádné milníky."))),
        h("section", { class: "card span-6" }, h("h3", {}, "CI (GitHub Actions)"),
          d.ci.length ? dataTable(["Workflow", t("Běhy"), t("Úspěšnost"), t("Průměr"), t("Poslední")], d.ci.map((c) => [c.workflow, c.runs, c.successRate == null ? "n/a" : Math.round(c.successRate * 100) + " %", fmt(Math.round(c.avgSeconds)) + " s", ciStatus(c.last)]), [1, 2, 3]) : empty(t("Žádné běhy."))),
        h("section", { class: "card span-6" }, h("h3", {}, t("Releasy")),
          d.releases.length ? dataTable(["Tag", t("Název"), t("Vydáno")], d.releases.map((r) => [r.tag + (r.prerelease ? " (pre)" : ""), r.name, fmtDate(r.published)])) : empty(t("Žádné releasy."))),
        h("section", { class: "card span-6" }, h("h3", {}, t("Jazyky a štítky")),
          d.languages.length ? languageBars(d) : empty(t("Bez údajů o jazycích.")),
          d.labels.length ? h("div", { class: "chips", style: "margin-top:12px" }, d.labels.map((l) => h("span", { class: "chip" }, l))) : null)),
    ];
  }

  function languageBars(d) {
    const total = sum(d.languages.map((l) => l.bytes)) || 1;
    const el = h("div", {});
    C.hBars(el, d.languages.slice(0, 8).map((l) => ({ label: l.lang, value: (l.bytes / total) * 100 })), { color: "var(--series-7)", format: (v) => C.nf1(v, 1) + " %" });
    return el;
  }

  const REPO_TABS = [["dev", "Vývoj"], ["people", "Lidé a moduly"], ["github", "GitHub"], ["agents", "Agenti"]];

  window.RL.pages.repo = async (parts, params) => {
    const days = window.RL.daysOf(params);
    const d = await api(`/api/repo/${encodeURIComponent(parts[0] || "")}` + window.RL.daysQuery(days));
    if (!d) return [h("h1", {}, t("Repozitář nenalezen")), h("p", {}, h("a", { href: "#/" }, t("← zpět na portfolio")))];
    const r = d.repo;
    const agentCommits = d.recent.filter((c) => c.agents || c.inSession).length;
    const defs = REPO_TABS.map(([k, l]) => [k, t(l)]);
    const tab = window.RL.tabOf(defs, params);
    const inPeriod = days === "all" ? "" : " · " + t("za {period}", { period: t(window.RL.PERIODS.find(([k]) => k === days)[1]) });
    const body = {
      dev: () => [h("div", { class: "grid" }, commitsCard(d), cumulativeCard(d), linesCard(d), issuesCard(d)),
        section(1, t("Poslední commity")), recentTable(d)],
      people: () => [h("div", { class: "grid" }, authorsCard(d), dirsCard(d))],
      github: () => githubTab(d),
      agents: () => window.RL.agentTabs(d.agents, params),
    }[tab]();
    return [
      h("p", {}, h("a", { href: "#/" }, "← " + t("Portfolio"))),
      h("div", { class: "kicker" }, r.key.startsWith("local:") ? t("lokální repozitář") : r.key),
      h("h1", {}, r.name),
      h("p", { class: "lede" }, r.description || r.localPath || ""),
      h("p", {}, sourceBadges(r), " ", h("span", { class: "note" }, t("Data: {when}", { when: window.RL.freshness(r) || "–" }))),
      h("div", { class: "tiles", style: "margin-top:16px" },
        tile(t("Commity") + inPeriod, fmt(r.commits), t("poslední {when}", { when: relTime(r.lastCommit) })),
        tile(t("Autoři") + inPeriod, fmt(d.authors.length)),
        tile([t("S agentem (90 dní)"), " ", window.RL.info(window.RL.AGENT_SHARE_HELP())], Math.round(r.agentShare * 100) + " %", t("trailer nebo session · {n} z posledních {total}", { n: agentCommits, total: d.recent.length })),
        tile(t("Otevřené issues / PR"), r.inGithub ? `${fmt(d.issueTimes.openIssues)} / ${fmt(d.issueTimes.openPrs)}` : "–", r.inGithub ? t("aktuálně") : null),
        tile(t("Hvězdy"), r.inGithub ? fmt(r.stars) : "–"),
        tile("Worktree", fmt(d.worktrees.length), d.worktrees.map((w) => t(w.branch)).slice(0, 3).join(", "))),
      window.RL.periodControl(params, t("Commity, řádky, autoři, adresáře, aktivita issues, CI a sessions za zvolené období; otevřené položky a releasy jsou aktuální.")),
      window.RL.tabs(defs, tab, params),
      ...body,
    ];
  };
})();
