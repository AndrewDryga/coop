/* coop site — players, copy buttons, nav, docs sidebar. Vanilla, no deps. */
(function () {
  "use strict";

  // --- asciinema players ---------------------------------------------------
  function mountCasts() {
    if (!window.AsciinemaPlayer) return;
    var FONT = 'ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace';
    document.querySelectorAll(".cast[data-cast]").forEach(function (el) {
      var src = el.getAttribute("data-cast");
      var auto = el.getAttribute("data-autoplay") === "true";
      var loop = el.getAttribute("data-loop") === "true";
      AsciinemaPlayer.create(src, el, {
        autoPlay: auto,
        loop: loop,
        preload: true,
        idleTimeLimit: 1.8,
        theme: "coop",
        fit: "width",
        controls: true,
        terminalFontFamily: FONT,
        terminalLineHeight: 1.35,
        poster: auto ? undefined : "npt:0:6"
      });
    });
  }

  // --- copy-to-clipboard ---------------------------------------------------
  function wireCopy() {
    document.querySelectorAll("[data-copy]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var target = document.querySelector(btn.getAttribute("data-copy"));
        if (!target) return;
        var text = target.textContent.trim();
        var done = function () {
          var prev = btn.textContent;
          btn.textContent = "Copied ✓";
          btn.classList.add("copied");
          setTimeout(function () {
            btn.textContent = prev;
            btn.classList.remove("copied");
          }, 1600);
        };
        if (navigator.clipboard) {
          navigator.clipboard.writeText(text).then(done, function () {});
        }
      });
    });
  }

  // --- mobile nav ----------------------------------------------------------
  function wireNav() {
    var toggle = document.querySelector("[data-nav-toggle]");
    var nav = document.querySelector("[data-nav]");
    if (!toggle || !nav) return;
    function setOpen(open) {
      nav.classList.toggle("open", open);
      toggle.setAttribute("aria-expanded", String(open));
    }
    toggle.addEventListener("click", function () { setOpen(!nav.classList.contains("open")); });
    nav.querySelectorAll("a").forEach(function (a) {
      a.addEventListener("click", function () { setOpen(false); });
    });
  }

  // --- docs sidebar: highlight the section in view -------------------------
  function wireDocsNav() {
    var side = document.querySelector(".docs-side");
    if (!side) return;
    var mobile = window.matchMedia("(max-width: 920px)");
    function syncOpen() { side.open = !mobile.matches; }
    syncOpen();
    mobile.addEventListener("change", syncOpen);
    var links = document.querySelectorAll('.docs-side a[href^="#"]');
    if (!links.length) return;
    links.forEach(function (a) {
      a.addEventListener("click", function () { if (mobile.matches) side.open = false; });
    });
    var map = {};
    links.forEach(function (a) { map[a.getAttribute("href").slice(1)] = a; });
    var obs = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (!e.isIntersecting) return;
        links.forEach(function (l) { l.classList.remove("active"); });
        if (map[e.target.id]) {
          map[e.target.id].classList.add("active");
          // Scroll only the desktop sidebar. scrollIntoView also moves the page,
          // interrupting anchor navigation; on mobile the contents are part of the page.
          var link = map[e.target.id];
          var side = link.parentElement;
          if (getComputedStyle(side).position === "sticky") {
            var item = link.getBoundingClientRect();
            var pane = side.getBoundingClientRect();
            if (item.top < pane.top) side.scrollTop += item.top - pane.top;
            else if (item.bottom > pane.bottom) side.scrollTop += item.bottom - pane.bottom;
          }
        }
      });
    }, { rootMargin: "-82px 0px -68% 0px", threshold: 0 });
    document.querySelectorAll(".doc-section[id]").forEach(function (s) { obs.observe(s); });
  }

  // Deep links land before the async-loaded casts size, which then shifts the page.
  // Re-scroll to the target once layout settles so a shared #anchor lands accurately.
  function settleHash() {
    if (!location.hash) return;
    var el;
    try { el = document.querySelector(location.hash); } catch (e) { return; }
    if (el) setTimeout(function () { el.scrollIntoView(); }, 450);
  }

  function init() {
    mountCasts();
    wireCopy();
    wireNav();
    wireDocsNav();
    settleHash();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
