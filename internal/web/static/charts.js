// Minimal SVG charts following the dataviz mark specs. Exposes window.Charts.
(function () {
  "use strict";
  const NS = "http://www.w3.org/2000/svg";
  const PAD = { top: 12, right: 12, bottom: 26, left: 44 };
  const BAR_MAX = 24;
  const GAP = 2;
  const RADIUS = 4;
  const TICKS = 4;
  const SHOW_POINTS_MAX = 12;
  const I = window.I18N;
  const t = I.t;
  // Number formats follow the UI language; formatters are cached per locale.
  const formatters = {};
  function numberFormat(digits) {
    const key = I.locale() + "|" + digits;
    if (!formatters[key]) formatters[key] = new Intl.NumberFormat(I.locale(), { minimumFractionDigits: digits, maximumFractionDigits: digits });
    return formatters[key];
  }
  const nf = { format: (v) => numberFormat(0).format(v) };
  const nf1 = (v, digits) => numberFormat(digits).format(v);

  // SVG presentation attributes do not reliably resolve var(); read the token's current value.
  function col(c) {
    const m = /^var\((--[\w-]+)\)$/.exec(c || "");
    return m ? getComputedStyle(document.documentElement).getPropertyValue(m[1]).trim() || c : c;
  }

  function svgEl(tag, attrs) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }

  // Clean axis: step is 1/2/5×10^k (at least 1) and top is the next multiple of step.
  function niceScale(v) {
    if (!(v > 0)) return { top: TICKS, step: 1 };
    const raw = v / TICKS;
    const p = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = Math.max(1, [1, 2, 5, 10].map((m) => m * p).find((x) => x >= raw));
    return { top: Math.ceil(v / step) * step, step };
  }

  function compact(v) {
    const a = Math.abs(v);
    const short = (x) => (Number.isInteger(Math.round(x * 10) / 10) ? nf.format(Math.round(x)) : nf1(x, 1));
    if (a >= 1e9) return short(v / 1e9) + " " + t("mld");
    if (a >= 1e6) return short(v / 1e6) + " M";
    if (a >= 1e4) return nf.format(Math.round(v / 1e3)) + " " + t("tis");
    return nf.format(Math.round(v));
  }

  // Bar with a rounded data-end and a square baseline end. dir=1 grows up, -1 down.
  function barPath(x, y0, w, h, dir, rounded) {
    if (h <= 0) return "";
    const r = rounded ? Math.min(RADIUS, w / 2, h) : 0;
    const yEnd = y0 - dir * h;
    if (dir > 0) {
      return `M${x},${y0}V${yEnd + r}Q${x},${yEnd} ${x + r},${yEnd}H${x + w - r}Q${x + w},${yEnd} ${x + w},${yEnd + r}V${y0}Z`;
    }
    return `M${x},${y0}V${yEnd - r}Q${x},${yEnd} ${x + r},${yEnd}H${x + w - r}Q${x + w},${yEnd} ${x + w},${yEnd - r}V${y0}Z`;
  }

  // Each chart is a labelled, focusable image; the label comes from the card title.
  function frame(container, height) {
    container.textContent = "";
    container.classList.add("chart");
    const width = Math.max(240, container.clientWidth || 600);
    const label = container.dataset.label || t("graf");
    const svg = svgEl("svg", { width, height, viewBox: `0 0 ${width} ${height}`, role: "img", "aria-label": label + " — " + t("šipkami procházejte hodnoty"), tabindex: 0 });
    const title = svgEl("title", {});
    title.textContent = label;
    svg.appendChild(title);
    container.appendChild(svg);
    const tip = document.createElement("div");
    tip.className = "tip";
    tip.hidden = true;
    container.appendChild(tip);
    return { svg, tip, width, height, iw: width - PAD.left - PAD.right, ih: height - PAD.top - PAD.bottom };
  }

  // Draws gridlines every `step` from lo to hi.
  function yAxis(f, hi, step, lo) {
    lo = lo || 0;
    const g = svgEl("g", { class: "axis" });
    for (let v = lo; v <= hi + step / 2; v += step) {
      const y = PAD.top + f.ih - ((v - lo) / (hi - lo)) * f.ih;
      g.appendChild(svgEl("line", { x1: PAD.left, x2: PAD.left + f.iw, y1: y, y2: y, class: v === 0 ? "base" : "grid" }));
      const t = svgEl("text", { x: PAD.left - 6, y: y + 4, "text-anchor": "end" });
      t.textContent = compact(v);
      g.appendChild(t);
    }
    f.svg.appendChild(g);
  }

  function xLabels(f, labels, xAt) {
    const g = svgEl("g", { class: "axis" });
    const n = labels.length;
    const picks = n <= 1 ? [0] : [0, Math.floor((n - 1) / 2), n - 1];
    const used = new Set();
    for (const i of picks) {
      if (used.has(i)) continue;
      used.add(i);
      const anchor = i === 0 ? "start" : i === n - 1 ? "end" : "middle";
      const t = svgEl("text", { x: xAt(i), y: f.height - 6, "text-anchor": anchor });
      t.textContent = labels[i];
      g.appendChild(t);
    }
    f.svg.appendChild(g);
  }

  function showTip(f, html, x, y) {
    f.tip.innerHTML = html;
    f.tip.hidden = false;
    const tw = f.tip.offsetWidth;
    const left = x + 14 + tw > f.width ? x - tw - 14 : x + 14;
    f.tip.style.left = Math.max(0, left) + "px";
    f.tip.style.top = Math.max(0, y - 10) + "px";
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  function tipRows(title, rows) {
    let h = `<div class="tip-title">${esc(title)}</div>`;
    for (const r of rows) {
      const key = r.color ? `<span class="key" style="background:${r.color}"></span>` : "";
      h += `<div class="tip-row">${key}<span>${esc(r.label)}</span><b>${esc(r.value)}</b></div>`;
    }
    return h;
  }

  // Hover band + pointer handling shared by bar and line charts.
  function hover(f, n, xAt, slot, onIndex) {
    const band = svgEl("rect", { class: "hover-band", y: PAD.top, height: f.ih, width: Math.max(slot, 2), visibility: "hidden" });
    f.svg.insertBefore(band, f.svg.firstChild);
    const hit = svgEl("rect", { x: PAD.left, y: PAD.top, width: f.iw, height: f.ih, fill: "transparent" });
    f.svg.appendChild(hit);
    const move = (ev) => {
      const rect = f.svg.getBoundingClientRect();
      const px = ev.clientX - rect.left;
      const i = Math.max(0, Math.min(n - 1, Math.round((px - PAD.left - slot / 2) / slot)));
      band.setAttribute("x", xAt(i) - slot / 2);
      band.setAttribute("visibility", "visible");
      onIndex(i, px, ev.clientY - rect.top);
    };
    const hide = () => {
      band.setAttribute("visibility", "hidden");
      f.tip.hidden = true;
      onIndex(-1);
    };
    hit.addEventListener("pointermove", move);
    hit.addEventListener("pointerleave", hide);
    // Keyboard: arrows move through the values, Escape hides the tooltip.
    let current = -1;
    const show = (i) => {
      current = Math.max(0, Math.min(n - 1, i));
      band.setAttribute("x", xAt(current) - slot / 2);
      band.setAttribute("visibility", "visible");
      onIndex(current, xAt(current), PAD.top + 10);
    };
    f.svg.addEventListener("keydown", (ev) => {
      const target = {
        Home: 0,
        End: n - 1,
        ArrowRight: current < 0 ? 0 : current + 1,
        ArrowLeft: current < 0 ? n - 1 : current - 1,
      }[ev.key];
      if (target !== undefined) {
        ev.preventDefault();
        show(target);
      } else if (ev.key === "Escape") {
        current = -1;
        hide();
      }
    });
    f.svg.addEventListener("blur", () => { current = -1; hide(); });
  }

  function legend(container, series) {
    if (series.length < 2) return;
    const l = document.createElement("div");
    l.className = "legend";
    for (const s of series) {
      const item = document.createElement("span");
      item.innerHTML = `<span class="key" style="background:${s.color}"></span>${esc(s.label)}`;
      l.appendChild(item);
    }
    container.appendChild(l);
  }

  // Stacked columns. opts: {labels, series:[{label,color,values}], height}
  function stackedBars(container, opts) {
    const f = frame(container, opts.height || 220);
    const n = opts.labels.length;
    const totals = opts.labels.map((_, i) => opts.series.reduce((a, s) => a + (s.values[i] || 0), 0));
    const { top: max, step } = niceScale(Math.max(0, ...totals));
    yAxis(f, max, step);
    const slot = f.iw / Math.max(n, 1);
    const bw = Math.max(1, Math.min(BAR_MAX, slot * 0.72));
    const xAt = (i) => PAD.left + slot * i + slot / 2;
    const y0 = PAD.top + f.ih;
    const g = svgEl("g", {});
    for (let i = 0; i < n; i++) {
      let acc = 0;
      const nonzero = opts.series.filter((s) => s.values[i] > 0);
      nonzero.forEach((s, k) => {
        const h = (s.values[i] / max) * f.ih;
        const gap = k > 0 && h > GAP ? GAP : 0;
        const d = barPath(xAt(i) - bw / 2, y0 - acc - gap, bw, h - gap, 1, k === nonzero.length - 1);
        if (d) g.appendChild(svgEl("path", { d, fill: col(s.color) }));
        acc += h;
      });
    }
    f.svg.appendChild(g);
    xLabels(f, opts.labels, xAt);
    hover(f, n, xAt, slot, (i, x, y) => {
      if (i < 0) return;
      const rows = opts.series.filter((s) => s.values[i] > 0).map((s) => ({ label: s.label, value: nf.format(s.values[i]), color: s.color }));
      rows.push({ label: t("celkem"), value: nf.format(totals[i]) });
      showTip(f, tipRows(opts.labels[i], rows), x, y);
    });
    legend(container, opts.series);
  }

  // Up/down columns around a zero baseline. opts: {labels, up:{label,color,values}, down:{...}}
  function divergingBars(container, opts) {
    const f = frame(container, opts.height || 200);
    const n = opts.labels.length;
    // Half as many ticks per side, so zero always lands on a gridline.
    const peak = Math.max(1, ...opts.up.values, ...opts.down.values);
    const step = niceScale(peak).step * 2;
    const half = Math.ceil(peak / step) * step;
    const max = 2 * half;
    yAxis(f, half, step, -half);
    const slot = f.iw / Math.max(n, 1);
    const bw = Math.max(1, Math.min(BAR_MAX, slot * 0.72));
    const xAt = (i) => PAD.left + slot * i + slot / 2;
    const zero = PAD.top + f.ih / 2;
    const scale = f.ih / max;
    const g = svgEl("g", {});
    for (let i = 0; i < n; i++) {
      const up = barPath(xAt(i) - bw / 2, zero - 1, bw, opts.up.values[i] * scale - 1, 1, true);
      const down = barPath(xAt(i) - bw / 2, zero + 1, bw, opts.down.values[i] * scale - 1, -1, true);
      if (up) g.appendChild(svgEl("path", { d: up, fill: col(opts.up.color) }));
      if (down) g.appendChild(svgEl("path", { d: down, fill: col(opts.down.color) }));
    }
    f.svg.appendChild(g);
    xLabels(f, opts.labels, xAt);
    hover(f, n, xAt, slot, (i, x, y) => {
      if (i < 0) return;
      showTip(f, tipRows(opts.labels[i], [
        { label: opts.up.label, value: "+" + nf.format(opts.up.values[i]), color: opts.up.color },
        { label: opts.down.label, value: "−" + nf.format(opts.down.values[i]), color: opts.down.color },
      ]), x, y);
    });
    legend(container, [opts.up, opts.down]);
  }

  // Lines with a crosshair. opts: {labels, series:[{label,color,values}], height, area}
  function lineChart(container, opts) {
    const f = frame(container, opts.height || 200);
    const n = opts.labels.length;
    const { top: max, step } = niceScale(Math.max(0, ...opts.series.flatMap((s) => s.values)));
    yAxis(f, max, step);
    const slot = f.iw / Math.max(n - 1, 1);
    const xAt = (i) => (n === 1 ? PAD.left + f.iw / 2 : PAD.left + slot * i);
    const yAt = (v) => PAD.top + f.ih - (v / max) * f.ih;
    const dots = [];
    for (const s of opts.series) {
      const pts = s.values.map((v, i) => `${xAt(i)},${yAt(v)}`);
      if (opts.area && n > 1) {
        f.svg.appendChild(svgEl("path", { d: `M${xAt(0)},${yAt(0)}L${pts.join("L")}L${xAt(n - 1)},${yAt(0)}Z`, fill: col(s.color), "fill-opacity": 0.1 }));
      }
      f.svg.appendChild(svgEl("path", { d: "M" + pts.join("L"), fill: "none", stroke: col(s.color), "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
      // Few points: show every marker so single values are readable.
      if (n <= SHOW_POINTS_MAX) {
        s.values.forEach((v, i) => f.svg.appendChild(svgEl("circle", { cx: xAt(i), cy: yAt(v), r: 4, fill: col(s.color), class: "dot" })));
      }
      const dot = svgEl("circle", { r: 4, fill: col(s.color), class: "dot", visibility: "hidden" });
      dots.push(dot);
      f.svg.appendChild(dot);
    }
    xLabels(f, opts.labels, xAt);
    hover(f, n, xAt, n === 1 ? f.iw : slot, (i, x, y) => {
      opts.series.forEach((s, k) => {
        if (i < 0) return dots[k].setAttribute("visibility", "hidden");
        dots[k].setAttribute("cx", xAt(i));
        dots[k].setAttribute("cy", yAt(s.values[i]));
        dots[k].setAttribute("visibility", "visible");
      });
      if (i < 0) return;
      showTip(f, tipRows(opts.labels[i], opts.series.map((s) => ({ label: s.label, value: nf.format(s.values[i]), color: s.color }))), x, y);
    });
    legend(container, opts.series);
  }

  // Horizontal single-series bars. rows: [{label, value, note}], opts: {color, format}
  function hBars(container, rows, opts) {
    container.textContent = "";
    container.classList.add("hbars");
    const max = Math.max(1, ...rows.map((r) => r.value));
    const fmt = (opts && opts.format) || compact;
    for (const r of rows) {
      const row = document.createElement("div");
      row.className = "hbar";
      row.title = r.note ? `${r.label}: ${fmt(r.value)} · ${r.note}` : `${r.label}: ${fmt(r.value)}`;
      row.innerHTML = `<span class="hbar-label">${esc(r.label)}</span><span class="hbar-track"><span class="hbar-fill" style="width:${(r.value / max) * 100}%;background:${(opts && opts.color) || "var(--series-1)"}"></span></span><span class="hbar-value">${esc(fmt(r.value))}</span>`;
      container.appendChild(row);
    }
  }

  // Inline sparkline as an SVG string; single series so no legend.
  function sparkline(values, color) {
    const w = 96, h = 24, n = values.length;
    const max = Math.max(1, ...values);
    const pts = values.map((v, i) => `${((i / Math.max(n - 1, 1)) * (w - 4) + 2).toFixed(1)},${(h - 2 - (v / max) * (h - 4)).toFixed(1)}`);
    const last = pts[pts.length - 1].split(",");
    color = col(color);
    return `<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true"><path d="M${pts.join("L")}" fill="none" stroke="${color}" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/><circle cx="${last[0]}" cy="${last[1]}" r="2.5" fill="${color}"/></svg>`;
  }

  window.Charts = { stackedBars, divergingBars, lineChart, hBars, sparkline, compact, esc, nf, nf1 };
})();
