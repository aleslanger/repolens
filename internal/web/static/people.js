// People page: authors across repositories with merged identities.
(function () {
  "use strict";
  const C = window.Charts;
  const { api, h, t, I, fmt, fmtDate, fmtDateTime, relTime, tile, section, chartCard, dataTable, empty, sum, localName } = window.RL;
  const TOP_PEOPLE = 4;
  const COLORS = ["var(--series-1)", "var(--series-2)", "var(--series-3)", "var(--series-4)", "var(--series-7)"];

  function monthlyCard(people) {
    const humans = people.filter((p) => !p.bot);
    const top = humans.slice(0, TOP_PEOPLE);
    const rest = people.filter((p) => !top.includes(p));
    const months = [...new Set(people.flatMap((p) => p.monthly.map((m) => m.month)))].sort();
    const series = top.map((p, i) => ({ label: p.name, color: COLORS[i], values: months.map((mo) => (p.monthly.find((m) => m.month === mo) || { count: 0 }).count) }));
    if (rest.length) {
      series.push({ label: t("ostatní a boti"), color: COLORS[TOP_PEOPLE], values: months.map((mo) => sum(rest.map((p) => (p.monthly.find((m) => m.month === mo) || { count: 0 }).count))) });
    }
    return chartCard({
      title: t("Commity po měsících"), note: t("Top {n} lidé, ostatní a boti sloučení.", { n: TOP_PEOPLE }),
      draw: (el) => (months.length ? C.stackedBars(el, { labels: months, series, height: 240 }) : el.replaceChildren(empty(t("Žádné commity.")))),
      table: { head: [t("Měsíc"), ...series.map((s) => s.label)], rows: months.map((mo, i) => [mo, ...series.map((s) => s.values[i])]), num: series.map((_, i) => i + 1) },
    });
  }

  function personRow(p) {
    const share = p.commits ? Math.round((p.agentCommits / p.commits) * 100) : 0;
    const details = h("details", {},
      h("summary", { class: "note" }, I.plural(p.identities.length, ["{n} identita", "{n} identity", "{n} identit"]) + " · " + I.plural(p.repos.length, ["{n} repozitář", "{n} repozitáře", "{n} repozitářů"])),
      dataTable([t("Jméno"), "E-mail", t("Sloučeno přes")], p.identities.map((i) => [i.name, i.email, i.via ? t(i.via) : "–"])),
      h("div", { class: "chips", style: "margin-top:8px" }, p.repos.slice(0, 40).map((r) => h("a", { class: "chip", href: `#/repo/${r.repoId}` }, `${r.name} ×${r.commits}`))));
    return h("tr", {},
      h("td", {}, p.personId ? h("a", { class: "repo-name", href: `#/person/${p.personId}` }, p.name) : h("strong", {}, p.name), p.bot ? h("span", { class: "badge", style: "margin-left:6px" }, t("bot / agent")) : null, details),
      h("td", { class: "num" }, fmt(p.commits)),
      h("td", { class: "num hide-sm" }, `+${C.compact(p.add)} / −${C.compact(p.del)}`),
      h("td", { class: "num" }, fmt(p.repos.length)),
      h("td", { class: "hide-sm" }, `${fmtDate(p.first)} – ${relTime(p.last)}`),
      h("td", {}, h("span", { class: "meter" }, h("span", { style: `width:${share}%` })), `${share} %`));
  }

  window.RL.pages.people = async (_, params) => {
    const days = window.RL.daysOf(params);
    const v = await api("/api/people" + window.RL.daysQuery(days));
    const people = v.people;
    const humans = people.filter((p) => !p.bot);
    const bots = people.filter((p) => p.bot);
    const merged = sum(people.map((p) => p.identities.length));
    return [
      h("div", { class: "hero" },
        h("div", {}, h("h1", { class: "kicker" }, t("Lidé napříč repozitáři")), h("div", { class: "hero-figure" }, fmt(humans.length), h("small", {}, t("lidí · {n} botů a agentů", { n: bots.length })))),
        h("div", { class: "tiles" },
          tile(t("Identit (jméno + e-mail)"), fmt(merged), t("sloučeno do lidí")),
          tile(t("Commity lidí"), fmt(sum(humans.map((p) => p.commits)))),
          tile(t("Commity botů"), fmt(sum(bots.map((p) => p.commits))), t("autor je bot nebo agent")))),
      h("p", { class: "lede", style: "margin-top:12px" }, t("Identity se slučují podle shodného e-mailu, jména bez diakritiky a GitHub loginu z noreply adresy. Boti se s lidmi nikdy neslučují automaticky. Ruční mapa je na stránce Nastavení.")),
      window.RL.periodControl(params, t("Commity, řádky a repozitáře za zvolené období.")),
      section(1, t("Aktivita")), h("div", { class: "grid" }, monthlyCard(people)),
      section(2, t("Lidé")),
      people.length ? h("div", { class: "table-wrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", {}, t("Kdo")), h("th", { class: "num" }, t("Commity")), h("th", { class: "num hide-sm" }, t("Řádky")), h("th", { class: "num" }, t("Repa")), h("th", { class: "hide-sm" }, t("Období")), h("th", {}, t("S agentem")))),
        h("tbody", {}, people.map(personRow)))) : empty(t("Žádní autoři.")),
    ];
  };

  // ---- one person -----------------------------------------------------------------------
  const C2 = window.Charts;
  window.RL.pages.person = async (parts, params) => {
    const days = window.RL.daysOf(params);
    const v = await api(`/api/person/${encodeURIComponent(parts[0] || "")}` + window.RL.daysQuery(days));
    if (!v) return [h("h1", {}, t("Osoba nenalezena")), h("p", {}, h("a", { href: "#/people" }, "← " + t("Lidé")))];
    const p = v.person;
    const share = p.commits ? Math.round((p.agentCommits / p.commits) * 100) : 0;
    const months = [...new Set(v.byRepo.map((x) => x.month))].sort();
    const topRepos = p.repos.slice(0, 6);
    const colors = ["var(--series-1)", "var(--series-2)", "var(--series-3)", "var(--series-4)", "var(--series-5)", "var(--series-6)", "var(--series-7)"];
    const series = topRepos.map((r, i) => ({ label: r.name, color: colors[i], values: months.map((m) => sum(v.byRepo.filter((x) => x.repoId === r.repoId && x.month === m).map((x) => x.count))) }));
    const restIDs = new Set(p.repos.slice(6).map((r) => r.repoId));
    if (restIDs.size) series.push({ label: t("ostatní"), color: colors[6], values: months.map((m) => sum(v.byRepo.filter((x) => restIDs.has(x.repoId) && x.month === m).map((x) => x.count))) });
    const types = Object.entries(v.types).sort((a, b) => b[1] - a[1]);
    return [
      h("p", {}, h("a", { href: "#/people" }, "← " + t("Lidé"))),
      h("div", { class: "kicker" }, p.bot ? t("bot / agent") : t("Osoba")),
      h("h1", {}, p.name),
      h("div", { class: "tiles", style: "margin-top:16px" },
        tile(t("Commity"), fmt(p.commits), `${fmtDate(p.first)} – ${relTime(p.last)}`),
        tile(t("Repa"), fmt(p.repos.length)),
        tile(t("Řádky"), `+${C2.compact(p.add)} / −${C2.compact(p.del)}`),
        tile([t("S agentem"), " ", window.RL.info(window.RL.AGENT_SHARE_HELP())], share + " %")),
      window.RL.periodControl(params, t("Commity, repozitáře a typy za zvolené období.")),
      section(1, t("Aktivita")),
      h("div", { class: "grid" }, chartCard({
        title: t("Commity po měsících a repozitářích"), note: t("Šest nejaktivnějších repozitářů, ostatní sloučené."),
        draw: (el) => (months.length ? C2.stackedBars(el, { labels: months, series, height: 240 }) : el.replaceChildren(empty(t("Žádné commity.")))),
        table: { head: [t("Měsíc"), ...series.map((s) => s.label)], rows: months.map((m, i) => [m, ...series.map((s) => s.values[i])]), num: series.map((_, i) => i + 1) },
      }), chartCard({
        title: t("Typy commitů"), span: 6,
        draw: (el) => (types.length ? C2.hBars(el, types.map(([k, n]) => ({ label: k, value: n })), { format: fmt }) : el.replaceChildren(empty(t("Žádné commity.")))),
        table: { head: [t("Typ"), t("Commity")], rows: types, num: [1] },
      }), h("section", { class: "card span-6" }, h("h3", {}, t("Repozitáře")),
        dataTable([t("Repozitář"), t("Commity"), t("Poslední")], p.repos.map((r) => [h("a", { href: `#/repo/${r.repoId}` }, r.name), r.commits, relTime(r.last)]), [1]))),
      section(2, t("Poslední commity")),
      v.recent.length ? dataTable([t("Kdy"), t("Repozitář"), t("Zpráva"), t("Typ"), "+", "−", t("Agent")],
        v.recent.map((c) => [h("span", { title: fmtDateTime(c.time), class: "nowrap" }, relTime(c.time)), h("a", { href: `#/repo/${c.repoId}` }, c.repo),
          h("span", { title: c.hash }, localName(c.subject)), c.type, c.add, c.del, c.agents || (c.inSession ? t("v session") : "–")]), [4, 5], t("Poslední commity")) : empty(t("Žádné commity.")),
      section(3, t("Identity")),
      dataTable([t("Jméno"), "E-mail", t("Sloučeno přes")], p.identities.map((i) => [i.name, i.email, i.via ? t(i.via) : "–"])),
    ];
  };
})();
