// repolens Data collection page: start a collection, its log, the errors of the latest run
// (with "exclude"), the run history, unrecognized records and a glossary.
(function () {
  "use strict";
  const { h, t, fmt, fmtDate, fmtDateTime, relTime, tile, section, dataTable, empty, localName, pages, api, send, problems } = window.RL;
  const RUN_MODES = { dir: "adresáře", github: "GitHub", all: "adresáře + GitHub" };

  // ---- collection run -------------------------------------------------------------------
  function collectCard(onDone) {
    const log = h("pre", { class: "log", "aria-live": "polite", tabindex: "0" });
    const state = h("p", { class: "field-hint" });
    const button = h("button", { type: "button", class: "btn primary" }, t("Spustit sběr"));
    let source = null;
    const follow = () => {
      if (source) source.close();
      log.textContent = "";
      button.disabled = true;
      state.textContent = t("Sběr běží…");
      source = new EventSource("/api/collect/log");
      source.addEventListener("line", (e) => {
        log.textContent += e.data + "\n";
        log.scrollTop = log.scrollHeight;
      });
      source.addEventListener("done", (e) => {
        source.close();
        source = null;
        button.disabled = false;
        let d = {};
        try { d = JSON.parse(e.data); } catch (err) { d = {}; }
        state.textContent = d.error ? t("Sběr skončil chybou: {err}", { err: d.error }) : t("Sběr dokončen.");
        window.RL.refreshLastRun();
        if (onDone) onDone();
      });
      source.onerror = () => {
        if (!source) return;
        source.close();
        source = null;
        button.disabled = false;
        state.textContent = t("Spojení s logem se přerušilo; obnov stránku.");
      };
    };
    button.addEventListener("click", async () => {
      button.disabled = true;
      const res = await send("POST", "/api/collect");
      if (res.status === 409 || res.ok) return follow();
      button.disabled = false;
      state.textContent = t("Sběr se nepodařilo spustit: {err}", { err: res.text.trim() || "HTTP " + res.status });
    });
    fetch("/api/collect", { headers: { Accept: "application/json" } }).then((r) => r.json()).then((s) => {
      if (s.running) follow();
      else if (s.finishedAt) state.textContent = t("Poslední sběr z dashboardu: {when}.", { when: relTime(s.finishedAt) });
    }).catch(() => { state.textContent = t("Stav sběru se nepodařilo zjistit."); });
    return h("section", { class: "card span-12" },
      h("div", { class: "card-head" }, h("div", {}, h("h3", {}, t("Sběr dat")),
        h("p", {}, t("Projde nastavené adresáře a GitHub účty, pak transcripty agentů. Stejné jako `repolens collect` bez argumentů; běží vždy jen jeden sběr."))),
        h("div", { class: "card-tools" }, button)),
      state, log);
  }

  // errorsBlock lists the latest run's errors; one that concerns a directory or a repository can
  // be excluded from later runs right here.
  function errorsBlock(errors, canEdit) {
    if (!errors.length) return empty(t("Poslední sběr proběhl bez chyb."));
    return h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, t("Chyba")), h("th", {}, t("Týká se")), h("th", {}, ""))),
      h("tbody", {}, errors.map((e) => {
        const result = h("span", { class: "field-hint" });
        const btn = canEdit && e.target ? h("button", { type: "button", class: "btn", onclick: async () => {
          btn.disabled = true;
          const r = await send("POST", "/api/exclude", { entry: e.target });
          if (r.ok) {
            btn.remove();
            result.replaceChildren(h("span", { class: "status good" }, t("Vyloučeno; projeví se při dalším sběru.")));
          } else {
            btn.disabled = false;
            result.textContent = problems(r).join(" ");
          }
        } }, t("Vyloučit")) : null;
        return h("tr", {}, h("td", { class: "mono" }, localName(e.message)), h("td", { class: "mono" }, e.target || "–"), h("td", {}, btn, result));
      }))));
  }

  // The glossary explains the terms the dashboard uses, in one place.
  const GLOSSARY = [
    ["Session", "Jedna práce s agentem (Claude Code nebo Codex) od spuštění do ukončení; obnovená session pokračuje pod stejným ID."],
    ["Vlákno", "Session, kterou spustil jiný agent (subagent Codexu). Její tokeny a cena se počítají k rodičovské session."],
    ["Okno aktivity", "Úsek session bez pauzy delší než 5 minut. Commit v okně (+10 min) se počítá jako commit s agentem."],
    ["Podíl agentů", "Podíl commitů za 90 dní s agentním trailerem nebo vzniklých v okně aktivity session ve stejném repu."],
    ["Tokeny", "Vstup + výstup. Čtení a zápis cache se ukazují zvlášť; opakují kontext a jsou levnější."],
    ["Cena", "Vždy odhad: co hlásí sám agent, jinak podle ceníku v Nastavení. Tokeny bez ceny se do ceny nepočítají."],
    ["Issue / PR", "Úkol nebo pull request z GitHubu; „otevřené“ je aktuální stav, ne stav za období."],
    ["Výjimka", "Adresář, který se neprochází, nebo repozitář, na který se repolens neptá GitHubu (lokální statistiky zůstanou)."],
  ];

  pages.status = async () => {
    const s = await api("/api/status");
    const last = s.runs[0];
    const live = !window.RL.BUNDLE;
    const view = [
      h("div", { class: "kicker" }, t("Sběr dat")),
      h("h1", {}, t("Sběr a kvalita dat")),
      h("p", { class: "lede" }, t("Statistiky commitů se počítají z lokálního git log; issues, PR, CI, releasy a hvězdy jdou z GitHub API; data o agentech z lokálních transcriptů (jen agregáty a jména). Nerozpoznané záznamy se nezahazují — počítají se níže.")),
    ];
    if (live) view.push(h("div", { class: "grid", style: "margin-top:16px" }, collectCard(() => window.RL.route())));
    view.push(
      last ? h("div", { class: "tiles", style: "margin-top:20px" },
        tile(t("Poslední běh"), fmtDate(last.startedAt), `${t(RUN_MODES[last.mode] || last.mode)}: ${localName(last.target)}`),
        tile(t("Repozitáře"), fmt(last.repos), t("{n} cizích přeskočeno", { n: fmt(last.skipped) })),
        tile("Sessions", fmt(last.sessions)),
        tile(t("Nerozpoznáno"), fmt(last.unrecognized)),
        tile(t("Chyby"), fmt(last.errors))) : empty(t("Zatím neproběhl žádný sběr. Spusť ho tlačítkem výše nebo příkazem `repolens collect`.")),
      section(1, t("Chyby posledního sběru")),
      h("p", { class: "note" }, t("Opakující se chybu (nečitelný adresář, repo, které GitHub už nezná) lze vyloučit; seznam výjimek je v Nastavení.")),
      errorsBlock(s.lastErrors || [], live),
      section(2, t("Běhy sběru")),
      dataTable([t("Začátek"), t("Režim"), t("Cíl"), t("Repa"), t("Přeskočeno"), t("Nové commity"), "Sessions", t("Nerozpoznáno"), t("Chyby")],
        s.runs.map((r) => [fmtDateTime(r.startedAt), t(RUN_MODES[r.mode] || r.mode), localName(r.target), r.repos, r.skipped, r.commits, r.sessions, r.unrecognized, r.errors]), [3, 4, 5, 6, 7, 8], t("Běhy sběru")),
      section(3, t("Nerozpoznané záznamy a chyby parsování")),
      s.issues.length ? dataTable([t("Zdroj"), t("Druh"), t("Co"), t("Počet")], s.issues.map((i) => [i.source, i.kind === "unrecognized" ? t("nerozpoznáno") : t("chyba"), localName(i.what), i.count]), [3], t("Nerozpoznané záznamy")) : empty(t("Žádné.")),
      section(4, t("Pojmy")),
      h("dl", { class: "glossary" }, GLOSSARY.flatMap(([term, text]) => [h("dt", {}, t(term)), h("dd", {}, t(text))])),
    );
    return view;
  };
})();
