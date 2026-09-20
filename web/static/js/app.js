/* DevPulse front-end: date filter behaviour and Chart.js wiring.
   All chart data is rendered server-side from PostgreSQL aggregates; this file
   never talks to GitHub. */
(function (global) {
  "use strict";

  var palette = {
    commits: "#1a73e8",
    additions: "#1e8e3e",
    deletions: "#d93025",
    prsOpened: "#8430ce",
    prsMerged: "#0b8043",
    reviews: "#e37400"
  };

  function baseOptions(stacked) {
    return {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: "index", intersect: false },
      plugins: {
        legend: { labels: { usePointStyle: true, boxWidth: 8, font: { family: "Roboto", size: 11 } } },
        tooltip: { padding: 10, cornerRadius: 8, titleFont: { family: "Roboto" }, bodyFont: { family: "Roboto" } }
      },
      scales: {
        x: { stacked: !!stacked, grid: { display: false }, ticks: { maxRotation: 0, autoSkipPadding: 18, font: { size: 10 } } },
        y: { stacked: !!stacked, beginAtZero: true, border: { display: false }, ticks: { precision: 0, font: { size: 10 } } }
      }
    };
  }

  function canvas(id) {
    var el = document.getElementById(id);
    if (!el || typeof Chart === "undefined") { return null; }
    return el;
  }

  function activityChart(id, series) {
    var el = canvas(id);
    if (!el || !series) { return; }
    new Chart(el, {
      type: "line",
      data: {
        labels: series.labels,
        datasets: [
          {
            label: "Commits", data: series.commits, borderColor: palette.commits,
            backgroundColor: "rgba(26,115,232,.14)", fill: true, tension: .35,
            pointRadius: 0, pointHoverRadius: 4, borderWidth: 2
          },
          {
            label: "Reviews", data: series.reviews, borderColor: palette.reviews,
            backgroundColor: "rgba(227,116,0,.10)", fill: true, tension: .35,
            pointRadius: 0, pointHoverRadius: 4, borderWidth: 2
          },
          {
            label: "PRs opened", data: series.prsOpened, borderColor: palette.prsOpened,
            backgroundColor: "transparent", tension: .35, pointRadius: 0, pointHoverRadius: 4,
            borderWidth: 2, borderDash: [5, 4]
          }
        ]
      },
      options: baseOptions(false)
    });
  }

  function prChart(id, series) {
    var el = canvas(id);
    if (!el || !series) { return; }
    new Chart(el, {
      type: "bar",
      data: {
        labels: series.labels,
        datasets: [
          { label: "PRs opened", data: series.prsOpened, backgroundColor: palette.prsOpened, borderRadius: 4 },
          { label: "PRs merged", data: series.prsMerged, backgroundColor: palette.prsMerged, borderRadius: 4 },
          { label: "Reviews", data: series.reviews, backgroundColor: palette.reviews, borderRadius: 4 }
        ]
      },
      options: baseOptions(true)
    });
  }

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

  document.addEventListener("DOMContentLoaded", initPeriodPicker);

  global.DevPulse = { activityChart: activityChart, prChart: prChart };
})(window);
