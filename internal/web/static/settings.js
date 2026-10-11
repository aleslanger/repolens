// repolens Settings page: configuration editor and the GitHub connection. Collections are
// started on the Data collection page.
(function () {
  "use strict";
  const { h, t, section, empty, fmt, pages, send, problems } = window.RL;
  const C = window.Charts;
  const MIN_DEPTH = 1;
  const MAX_DEPTH = 10;

  const splitList = (s) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
  const numOr = (v, d) => (v === "" || isNaN(Number(v)) ? d : Number(v));

  function field(label, input, hint) {
    return h("label", { class: "field" }, h("span", { class: "field-label" }, label), input, hint ? h("span", { class: "field-hint" }, hint) : null);
  }
  function checkbox(label, checked, hint) {
    const input = h("input", { type: "checkbox", checked: !!checked });
    return { input, el: h("label", { class: "check" }, input, h("span", {}, label, hint ? h("span", { class: "field-hint" }, hint) : null)) };
  }
  function removeButton(label, onClick) {
    return h("button", { type: "button", class: "btn ghost", "aria-label": label, title: label, onclick: onClick }, "×");
  }

  // ---- editable lists -------------------------------------------------------------
  function rootsEditor(roots) {
    const list = h("div", { class: "rows" });
    const add = (r) => {
      const path = h("input", { type: "text", value: r.path || "", placeholder: "~/projects", "aria-label": t("Cesta") });
      const depth = h("input", { type: "number", min: MIN_DEPTH, max: MAX_DEPTH, value: r.depth || "", placeholder: "4", "aria-label": t("Hloubka") });
      const row = h("div", { class: "row" }, path, depth, removeButton(t("Odebrat"), () => row.remove()));
      row.read = () => (path.value.trim() ? { path: path.value.trim(), depth: numOr(depth.value, 0) } : null);
      list.appendChild(row);
    };
    roots.forEach(add);
    const el = h("div", {}, h("div", { class: "rows-head" }, h("span", {}, t("Cesta")), h("span", {}, t("Hloubka"))), list,
      h("button", { type: "button", class: "btn", onclick: () => add({}) }, t("Přidat adresář")));
    return { el, read: () => [...list.children].map((r) => r.read()).filter(Boolean) };
  }

  function identitiesEditor(ids) {
    const list = h("div", { class: "rows" });
    const add = (p) => {
      const name = h("input", { type: "text", value: p.name || "", placeholder: t("Jméno"), "aria-label": t("Jméno") });
      const aliases = h("input", { type: "text", value: (p.aliases || []).join(", "), placeholder: t("e-maily, loginy, jiná jména"), "aria-label": t("Aliasy") });
      const row = h("div", { class: "row wide" }, name, aliases, removeButton(t("Odebrat"), () => row.remove()));
      // Aliases are comma-separated; a name alias may contain spaces ("Octo Cat").
      row.read = () => (name.value.trim() ? { name: name.value.trim(), aliases: aliases.value.split(",").map((x) => x.trim()).filter(Boolean) } : null);
      list.appendChild(row);
    };
    ids.forEach(add);
    const el = h("div", {}, list, h("button", { type: "button", class: "btn", onclick: () => add({}) }, t("Přidat osobu")));
    return { el, read: () => [...list.children].map((r) => r.read()).filter(Boolean) };
  }

  const PRICE_KEYS = [["input", "vstup"], ["output", "výstup"], ["cache_read", "čtení cache"], ["cache_write", "zápis cache"]];
  function pricesEditor(prices, unpriced) {
    const list = h("div", { class: "rows" });
    const add = (model, p) => {
      const name = h("input", { type: "text", value: model || "", placeholder: "gpt-5.5", "aria-label": t("Model") });
      const inputs = PRICE_KEYS.map(([k, label]) => h("input", { type: "number", min: 0, step: "any", value: p && p[k] != null ? p[k] : "", placeholder: "0", "aria-label": t(label) }));
      const row = h("div", { class: "row prices" }, name, ...inputs, removeButton(t("Odebrat"), () => row.remove()));
      row.read = () => {
        if (!name.value.trim()) return null;
        const price = {};
        PRICE_KEYS.forEach(([k], i) => (price[k] = numOr(inputs[i].value, 0)));
        return [name.value.trim(), price];
      };
      list.appendChild(row);
    };
    Object.entries(prices || {}).forEach(([m, p]) => add(m, p));
    // Models seen without any price; adding one pre-fills the name only, prices are the user's.
    const missing = (unpriced || []).filter((m) => !Object.keys(prices || {}).some((k) => m.model.startsWith(k)));
    const suggest = missing.length ? h("div", { class: "field-hint" }, t("Modely bez ceny:") + " ",
      ...missing.map((m) => h("button", { type: "button", class: "btn", style: "margin:2px 4px 2px 0", title: t("{n} tokenů bez ceny", { n: C.compact(m.tokens) }),
        onclick: (e) => { add(m.model, null); e.currentTarget.remove(); } }, "+ " + m.model))) : null;
    const el = h("div", {}, suggest,
      h("div", { class: "rows-head prices" }, h("span", {}, t("Model")), ...PRICE_KEYS.map(([, l]) => h("span", {}, t(l))), h("span", {})),
      list, h("button", { type: "button", class: "btn", onclick: () => add("", null) }, t("Přidat model")));
    // set replaces the rows, used when a price list is imported.
    const set = (map) => {
      list.replaceChildren();
      Object.entries(map).forEach(([m, p]) => add(m, p));
    };
    return { el, set, read: () => Object.fromEntries([...list.children].map((r) => r.read()).filter(Boolean)) };
  }

  // listEditor edits a list of single values (exclusions) or pairs (session paths), one per row.
  function listEditor(values, columns) {
    const list = h("div", { class: "rows" });
    const add = (vals) => {
      const inputs = columns.map(([placeholder, label], i) => h("input", { type: "text", value: vals[i] || "", placeholder, "aria-label": t(label) }));
      const row = h("div", { class: "row " + (columns.length > 1 ? "wide" : "single") }, ...inputs, h("button", { type: "button", class: "btn ghost", "aria-label": t("Odebrat"), title: t("Odebrat"), onclick: () => row.remove() }, "×"));
      row.read = () => (inputs[0].value.trim() ? inputs.map((x) => x.value.trim()) : null);
      list.appendChild(row);
    };
    values.forEach(add);
    const el = h("div", {}, list, h("button", { type: "button", class: "btn", onclick: () => add([]) }, t("Přidat")));
    return { el, read: () => [...list.children].map((r) => r.read()).filter(Boolean) };
  }

  // importPrices reads a pasted price list: {"date": "2026-10-01", "prices": {model: {input, …}}}.
  // repolens never makes prices up; the list and its date come from the user.
  function importBox(prices, date) {
    const area = h("textarea", { rows: 4, placeholder: '{"date": "2026-10-01", "prices": {"gpt-5.5": {"input": 1.25, "output": 10, "cache_read": 0.125, "cache_write": 0}}}', "aria-label": t("Ceník v JSON") });
    const msg = h("span", { class: "field-hint" });
    const btn = h("button", { type: "button", class: "btn", onclick: () => {
      let data;
      try { data = JSON.parse(area.value); } catch (e) { msg.textContent = t("Neplatný JSON."); return; }
      const list = data && typeof data.prices === "object" ? data.prices : null;
      if (!list || !/^\d{4}-\d{2}-\d{2}$/.test(data.date || "")) {
        msg.textContent = t("Čekám objekt s klíči „date“ (RRRR-MM-DD) a „prices“.");
        return;
      }
      prices.set(Object.assign(Object.fromEntries(Object.entries(prices.read())), list));
      date.value = data.date;
      msg.textContent = t("Načteno {n} modelů k {date}; zkontroluj a ulož.", { n: Object.keys(list).length, date: data.date });
    } }, t("Načíst ceník"));
    return h("details", {}, h("summary", { class: "note" }, t("Vložit ceník (JSON)")), area, h("div", { class: "form-actions", style: "margin-top:8px" }, btn, msg));
  }

  // ---- GitHub connection ----------------------------------------------------------------
  function githubCard() {
    const body = h("div", { class: "gh-status" }, h("p", { class: "loading" }, t("Ověřuji…")));
    const check = async () => {
      body.replaceChildren(h("p", { class: "loading" }, t("Ověřuji…")));
      try {
        const res = await fetch("/api/github", { headers: { Accept: "application/json" } });
        if (!res.ok) throw new Error("HTTP " + res.status);
        body.replaceChildren(...githubRows(await res.json()));
      } catch (err) {
        body.replaceChildren(h("div", { class: "notice" }, t("Stav GitHubu se nepodařilo zjistit: {err}", { err: err.message })));
      }
    };
    check();
    return h("section", { class: "card span-12" },
      h("div", { class: "card-head" }, h("div", {}, h("h3", {}, t("Připojení ke GitHubu")),
        h("p", {}, t("Token se bere z GITHUB_TOKEN nebo z `gh auth token`; nikdy se neukládá ani neposílá do prohlížeče."))),
        h("div", { class: "card-tools" }, h("button", { type: "button", class: "btn", onclick: check }, t("Ověřit")))),
      body);
  }

  function githubRows(s) {
    if (!s.source) {
      return [h("p", {}, h("span", { class: "status critical" }, t("nepřipojeno")), " ", s.error ? t("Token není k dispozici.") : ""),
        h("p", { class: "field-hint" }, t("Přihlas se příkazem `gh auth login`, nebo nastav proměnnou GITHUB_TOKEN a restartuj server."))];
    }
    if (!s.viewer) {
      return [h("p", {}, h("span", { class: "status critical" }, t("chyba")), " ", t("Zdroj tokenu: {src}", { src: s.source })), h("pre", { class: "log" }, s.error || "")];
    }
    const v = s.viewer;
    const limit = (l) => t("{rem} z {lim}, obnova {when}", { rem: fmt(l.remaining), lim: fmt(l.limit), when: new Date(l.reset).toLocaleTimeString(window.I18N.locale()) });
    return [h("dl", { class: "facts" },
      h("dt", {}, t("Stav")), h("dd", {}, h("span", { class: "status good" }, t("připojeno"))),
      h("dt", {}, t("Účet")), h("dd", {}, v.login),
      h("dt", {}, t("Zdroj tokenu")), h("dd", {}, s.source),
      h("dt", {}, t("Oprávnění")), h("dd", {}, v.scopes == null ? t("token je neuvádí (fine-grained nebo GitHub App)") : v.scopes.length ? v.scopes.join(", ") : t("žádná")),
      h("dt", {}, t("Limit REST")), h("dd", {}, limit(v.core)),
      h("dt", {}, t("Limit GraphQL")), h("dd", {}, limit(v.graphql)))];
  }

  // ---- page ----------------------------------------------------------------------------
  pages.settings = async () => {
    if (window.RL.BUNDLE) {
      return [h("div", { class: "kicker" }, t("Nastavení")), h("h1", {}, t("Nastavení")),
        empty(t("Statický export nastavení nemá. Spusť `repolens serve` a otevři dashboard na localhostu."))];
    }
    const res = await fetch("/api/settings", { headers: { Accept: "application/json" } });
    if (!res.ok) throw new Error("/api/settings: HTTP " + res.status);
    const { path, values: v, unpriced } = await res.json();
    const roots = rootsEditor(v.roots || []);
    const accounts = h("input", { type: "text", value: (v.github_accounts || []).join(", "), placeholder: "octo, octo-org" });
    const owners = h("input", { type: "text", value: (v.owners || []).join(", "), placeholder: "github.com/octo" });
    const uncloned = checkbox(t("Sbírat i repa bez lokálního klonu"), v.include_uncloned !== false, t("issues, PR, CI a historie commitů z GitHub API"));
    const forks = checkbox(t("Zahrnout forky"), v.include_forks);
    const archived = checkbox(t("Zahrnout archivované"), v.include_archived);
    const ciDays = h("input", { type: "number", min: 1, max: 3650, value: v.ci_days || 90 });
    const identities = identitiesEditor(v.identities || []);
    const prices = pricesEditor(v.prices, unpriced);
    const pricesDate = h("input", { type: "date", value: v.prices_date || "", "aria-label": t("Ceník platí k") });
    const fetchPrices = checkbox(t("Doplňovat chybějící ceny z veřejného ceníku LiteLLM"), v.fetch_prices,
      t("Volitelné. Při sběru se ceník stáhne přes GitHub API (repozitář BerriAI/litellm, jediné síťové spojení repolens). Tvoje ceny výše mají vždy přednost; ceny, které hlásí sám agent, se nemění."));
    const fetchMsg = h("span", { class: "field-hint" });
    const fetchBtn = h("button", { type: "button", class: "btn", onclick: async () => {
      fetchBtn.disabled = true;
      fetchMsg.textContent = t("Stahuji ceník…");
      const r = await send("POST", "/api/prices/fetch");
      fetchBtn.disabled = false;
      if (!r.ok) {
        fetchMsg.textContent = t("Ceník se nepodařilo stáhnout: {err}", { err: problems(r).join(" ") });
        return;
      }
      // Only missing models are added; prices already in the form stay as the user set them.
      const current = prices.read();
      const added = Object.entries(r.data.prices).filter(([m]) => !(m in current));
      prices.set(Object.assign(Object.fromEntries(added), current));
      if (added.length && !pricesDate.value) pricesDate.value = r.data.date;
      fetchMsg.textContent = t("{source} k {date}: doplněno {n} modelů; zkontroluj a ulož.", { source: r.data.source, date: r.data.date, n: added.length });
    } }, t("Načíst ceny používaných modelů z GitHubu"));
    const exclude = listEditor((v.exclude || []).map((x) => [x]), [["~/projects/data nebo github.com/octo/old", "Výjimka"]]);
    const sessionPaths = listEditor(Object.entries(v.session_paths || {}), [["~/old/app", "Adresář sessions"], ["github.com/octo/app", "Repozitář"]]);
    const result = h("div", { class: "save-result", role: "status" });
    const saveBtn = h("button", { type: "submit", class: "btn primary" }, t("Uložit nastavení"));

    const form = h("form", { class: "settings", novalidate: true },
      section(1, t("Co sbírat")),
      h("div", { class: "card span-12 form-card" },
        h("h3", {}, t("Lokální adresáře")),
        h("p", { class: "field-hint" }, t("Kořeny, ve kterých se hledají git repozitáře. Hloubka 1–10, prázdná = výchozí.")),
        roots.el,
        h("h3", {}, t("GitHub")),
        field(t("Účty a organizace"), accounts, t("Oddělené čárkou. Prázdné = vlastníci z github.com níže.")),
        uncloned.el, forks.el, archived.el,
        field(t("Vlastní repozitáře"), owners, t("host/účet, oddělené čárkou. Repa jiných vlastníků se přeskočí a jen spočítají; prázdné = všechna.")),
        field(t("Historie CI (dní)"), ciDays, t("Jak daleko zpět se při prvním sběru načtou běhy GitHub Actions.")),
        h("h3", {}, t("Výjimky")),
        h("p", { class: "field-hint" }, t("Adresáře, které se nemají procházet (např. data databází), a repozitáře, na které se nemá ptát GitHubu (lokální statistiky zůstanou). Chybu lze vyloučit i přímo na stránce Sběr dat.")),
        exclude.el,
        h("h3", {}, t("Přiřazení sessions")),
        h("p", { class: "field-hint" }, t("Adresář sessions, který nepatří žádnému repozitáři (přesunutý nebo smazaný klon), a repozitář, ke kterému se má počítat.")),
        sessionPaths.el),
      section(2, t("Identity")),
      h("div", { class: "card span-12 form-card" },
        h("p", { class: "field-hint" }, t("Ruční sloučení autorů: jméno a jeho aliasy (e-maily, GitHub loginy, jiné podoby jména).")),
        identities.el),
      section(3, t("Ceník modelů")),
      h("div", { class: "card span-12 form-card" },
        h("p", { class: "field-hint" }, t("USD za milion tokenů pro modely, u kterých agent cenu nehlásí (klíč = celý název nebo prefix). repolens ceny nevymýšlí: doplň je podle aktuálního ceníku poskytovatele.")),
        prices.el,
        field(t("Ceník platí k"), pricesDate, t("Datum se ukazuje u odhadů ceny.")),
        fetchPrices.el,
        h("div", { class: "form-actions", style: "margin-top:0" }, fetchBtn, fetchMsg),
        importBox(prices, pricesDate)),
      h("div", { class: "form-actions" }, saveBtn, h("span", { class: "field-hint" }, t("Ukládá se do {path}; předchozí verze zůstane jako .bak.", { path })), result));

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      saveBtn.disabled = true;
      result.replaceChildren();
      const body = {
        roots: roots.read(), github_accounts: splitList(accounts.value), owners: splitList(owners.value),
        include_uncloned: uncloned.input.checked, include_forks: forks.input.checked, include_archived: archived.input.checked,
        ci_days: numOr(ciDays.value, 0), identities: identities.read(), prices: prices.read(), prices_date: pricesDate.value, fetch_prices: fetchPrices.input.checked,
        exclude: exclude.read().map((r) => r[0]), session_paths: Object.fromEntries(sessionPaths.read().map((r) => [r[0], r[1] || ""])),
      };
      try {
        const r = await send("PUT", "/api/settings", body);
        if (r.ok) result.replaceChildren(h("span", { class: "status good" }, t("Uloženo")));
        else if (r.status === 422) result.replaceChildren(h("div", { class: "notice" }, t("Nastavení nebylo uloženo:"), h("ul", {}, problems(r).map((m) => h("li", {}, m)))));
        else result.replaceChildren(h("div", { class: "notice" }, t("Uložení selhalo: {err}", { err: r.text.trim() || "HTTP " + r.status })));
      } catch (err) {
        result.replaceChildren(h("div", { class: "notice" }, t("Uložení selhalo: {err}", { err: err.message })));
      } finally {
        saveBtn.disabled = false;
      }
    });

    return [
      h("div", { class: "kicker" }, t("Nastavení")),
      h("h1", {}, t("Zdroje dat")),
      h("p", { class: "lede" }, t("Změny se projeví při dalším sběru (spouští se na stránce Sběr dat). Konfigurace neobsahuje žádná tajemství; GitHub token zůstává v gh CLI nebo v proměnné prostředí.")),
      h("div", { class: "grid" }, githubCard()),
      form,
    ];
  };
})();
