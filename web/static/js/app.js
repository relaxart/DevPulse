/* DevPulse front-end: colour theme, date filter behaviour and Chart.js wiring.
   All chart data is rendered server-side from PostgreSQL aggregates; this file
   never talks to GitHub. */
(function (global) {
  "use strict";

  var STORAGE_KEY = "devpulse-theme";
  var root = document.documentElement;

  // ---------------------------------------------------------------- theme ---

  function systemPrefersDark() {
    return global.matchMedia && global.matchMedia("(prefers-color-scheme: dark)").matches;
  }

  function storedMode() {
    try {
      var v = global.localStorage.getItem(STORAGE_KEY);
      return (v === "light" || v === "dark") ? v : "auto";
    } catch (e) {
      // Storage can be unavailable (private mode, blocked cookies).
      return "auto";
    }
  }

  function storeMode(mode) {
    try {
      if (mode === "auto") {
        global.localStorage.removeItem(STORAGE_KEY);
      } else {
        global.localStorage.setItem(STORAGE_KEY, mode);
      }
    } catch (e) {
      // Not being able to remember the choice must not break the toggle.
    }
  }

  function isDark(mode) {
    return mode === "dark" || (mode === "auto" && systemPrefersDark());
  }

  function applyMode(mode) {
    root.setAttribute("data-bs-theme", isDark(mode) ? "dark" : "light");
    root.setAttribute("data-theme-mode", mode);
    updateThemeControls(mode);
    renderAllCharts();
  }

  var THEME_ICONS = { light: "light_mode", dark: "dark_mode", auto: "contrast" };

  function updateThemeControls(mode) {
    document.querySelectorAll("[data-theme-icon]").forEach(function (el) {
      el.textContent = THEME_ICONS[mode] || THEME_ICONS.auto;
    });
    document.querySelectorAll("[data-theme-choice]").forEach(function (el) {
      el.setAttribute("aria-checked", el.getAttribute("data-theme-choice") === mode ? "true" : "false");
    });
  }

  function initTheme() {
    var mode = storedMode();
    applyMode(mode);

    document.querySelectorAll("[data-theme-choice]").forEach(function (el) {
      el.addEventListener("click", function () {
        var chosen = el.getAttribute("data-theme-choice");
        storeMode(chosen);
        applyMode(chosen);
      });
    });

    // Follow the operating system while the mode is "auto".
    if (global.matchMedia) {
      var query = global.matchMedia("(prefers-color-scheme: dark)");
      var onChange = function () {
        if (storedMode() === "auto") { applyMode("auto"); }
      };
      if (query.addEventListener) {
        query.addEventListener("change", onChange);
      } else if (query.addListener) {
        query.addListener(onChange);
      }
    }
  }

  // --------------------------------------------------------------- charts ---

  // Palettes are per theme: the light hues are too dark to read on a dark
  // surface, and the dark ones wash out on white.
  var PALETTES = {
    light: {
      commits: "#1a73e8", commitsFill: "rgba(26,115,232,.14)",
      prs: "#8430ce", prsMerged: "#0b8043",
      reviews: "#e37400", reviewsFill: "rgba(227,116,0,.10)",
      text: "#5f6672", grid: "rgba(16,24,40,.08)",
      tooltipBg: "rgba(31,36,48,.92)", tooltipText: "#ffffff"
    },
    dark: {
      commits: "#8ab4f8", commitsFill: "rgba(138,180,248,.16)",
      prs: "#c58af9", prsMerged: "#81c995",
      reviews: "#fcad70", reviewsFill: "rgba(252,173,112,.12)",
      text: "#9aa3b2", grid: "rgba(255,255,255,.10)",
      tooltipBg: "rgba(232,236,243,.94)", tooltipText: "#10131a"
    }
  };

  function palette() {
    return root.getAttribute("data-bs-theme") === "dark" ? PALETTES.dark : PALETTES.light;
  }

  // registry keeps enough information to rebuild every chart when the theme
  // changes, which is simpler and more reliable than patching option trees.
  var registry = [];
  var instances = {};

  function baseOptions(stacked) {
    var c = palette();
    return {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: "index", intersect: false },
      plugins: {
        legend: {
          labels: {
            usePointStyle: true, boxWidth: 8, color: c.text,
            font: { family: "Roboto", size: 11 }
          }
        },
        tooltip: {
          padding: 10, cornerRadius: 8,
          backgroundColor: c.tooltipBg, titleColor: c.tooltipText, bodyColor: c.tooltipText,
          titleFont: { family: "Roboto" }, bodyFont: { family: "Roboto" }
        }
      },
      scales: {
        x: {
          stacked: !!stacked,
          grid: { display: false },
          border: { color: c.grid },
          ticks: { color: c.text, maxRotation: 0, autoSkipPadding: 18, font: { size: 10 } }
        },
        y: {
          stacked: !!stacked,
          beginAtZero: true,
          grid: { color: c.grid },
          border: { display: false },
          ticks: { color: c.text, precision: 0, font: { size: 10 } }
        }
      }
    };
  }

  function buildActivity(series) {
    var c = palette();
    return {
      type: "line",
      data: {
        labels: series.labels,
        datasets: [
          {
            label: "Commits", data: series.commits, borderColor: c.commits,
            backgroundColor: c.commitsFill, fill: true, tension: .35,
            pointRadius: 0, pointHoverRadius: 4, borderWidth: 2
          },
          {
            label: "Reviews", data: series.reviews, borderColor: c.reviews,
            backgroundColor: c.reviewsFill, fill: true, tension: .35,
            pointRadius: 0, pointHoverRadius: 4, borderWidth: 2
          },
          {
            label: "PRs opened", data: series.prsOpened, borderColor: c.prs,
            backgroundColor: "transparent", tension: .35, pointRadius: 0, pointHoverRadius: 4,
            borderWidth: 2, borderDash: [5, 4]
          }
        ]
      },
      options: baseOptions(false)
    };
  }

  function buildPR(series) {
    var c = palette();
    return {
      type: "bar",
      data: {
        labels: series.labels,
        datasets: [
          { label: "PRs opened", data: series.prsOpened, backgroundColor: c.prs, borderRadius: 4 },
          { label: "PRs merged", data: series.prsMerged, backgroundColor: c.prsMerged, borderRadius: 4 },
          { label: "Reviews", data: series.reviews, backgroundColor: c.reviews, borderRadius: 4 }
        ]
      },
      options: baseOptions(true)
    };
  }

  var BUILDERS = { activity: buildActivity, pr: buildPR };

  function renderChart(entry) {
    var el = document.getElementById(entry.id);
    if (!el || typeof Chart === "undefined" || !entry.series) { return; }
    if (instances[entry.id]) {
      instances[entry.id].destroy();
    }
    instances[entry.id] = new Chart(el, BUILDERS[entry.kind](entry.series));
  }

  function renderAllCharts() {
    registry.forEach(renderChart);
  }

  function register(kind, id, series) {
    registry = registry.filter(function (e) { return e.id !== id; });
    registry.push({ kind: kind, id: id, series: series });
    // Charts are registered from inline scripts in <body>; wait for the canvas
    // and the Chart.js bundle to be present.
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", function () { renderChart(registry[registry.length - 1]); });
    } else {
      renderChart(registry[registry.length - 1]);
    }
  }

  // ---------------------------------------------------------- date filter ---

  // Show the custom date inputs only when "Custom range" is selected.
  function initPeriodPicker() {
    document.querySelectorAll("[data-period-select]").forEach(function (select) {
      var form = select.closest("form");
      if (!form) { return; }
      var custom = form.querySelector("[data-custom-range]");
      select.addEventListener("change", function () {
        if (!custom) { return; }
        if (select.value === "custom") {
          custom.classList.remove("d-none");
        } else {
          custom.classList.add("d-none");
          form.submit();
        }
      });
    });
  }

  function init() {
    initTheme();
    initPeriodPicker();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }

  global.DevPulse = {
    activityChart: function (id, series) { register("activity", id, series); },
    prChart: function (id, series) { register("pr", id, series); },
    setTheme: function (mode) { storeMode(mode); applyMode(mode); },
    theme: function () { return storedMode(); }
  };
})(window);
