/* coop site — Mixpanel: autocapture + session replay, plus a named install-copy event. */
(function () {
  "use strict";

  // --- Mixpanel loader (official snippet) ----------------------------------
  // Installs the window.mixpanel stub synchronously, then async-loads the SDK,
  // so any .init()/.track() calls below queue until the real library arrives.
  // prettier-ignore
  (function (f, b) { if (!b.__SV) { var e, g, i, h; window.mixpanel = b; b._i = []; b.init = function (e, f, c) { function g(a, d) { var b = d.split("."); 2 == b.length && ((a = a[b[0]]), (d = b[1])); a[d] = function () { a.push([d].concat(Array.prototype.slice.call(arguments, 0))); }; } var a = b; "undefined" !== typeof c ? (a = b[c] = []) : (c = "mixpanel"); a.people = a.people || []; a.toString = function (a) { var d = "mixpanel"; "mixpanel" !== c && (d += "." + c); a || (d += " (stub)"); return d; }; a.people.toString = function () { return a.toString(1) + ".people (stub)"; }; i = "disable time_event track track_pageview track_links track_forms track_with_groups add_group set_group remove_group register register_once alias unregister identify name_tag set_config reset opt_in_tracking opt_out_tracking has_opted_in_tracking has_opted_out_tracking clear_opt_in_out_tracking start_batch_senders people.set people.set_once people.unset people.increment people.append people.union people.track_charge people.clear_charges people.delete_user people.remove".split(" "); for (h = 0; h < i.length; h++) g(a, i[h]); var j = "set set_once union unset remove delete".split(" "); a.get_group = function () { function b(c) { d[c] = function () { call2_args = arguments; call2 = [c].concat(Array.prototype.slice.call(call2_args, 0)); a.push([e, call2]); }; } for (var d = {}, e = ["get_group"].concat(Array.prototype.slice.call(arguments, 0)), c = 0; c < j.length; c++) b(j[c]); return d; }; b._i.push([e, f, c]); }; b.__SV = 1.2; e = f.createElement("script"); e.type = "text/javascript"; e.async = !0; e.src = "//cdn.mxpnl.com/libs/mixpanel-2-latest.min.js"; g = f.getElementsByTagName("script")[0]; g.parentNode.insertBefore(e, g); } })(document, window.mixpanel || []);

  // --- init ----------------------------------------------------------------
  mixpanel.init("e5e1486473fb822600c835e9cc58d27e", {
    autocapture: true, // clicks, scrolls, form submits, and pageviews — no manual wiring
    track_pageview: true, // page views across the landing + docs pages
    persistence: "localStorage", // no tracking cookies
    record_sessions_percent: 100, // session replay (low-traffic public site; text + inputs are masked by default)
    debug: /^(localhost|127\.0\.0\.1)$/.test(location.hostname),
  });

  // --- install command copied ----------------------------------------------
  // The page's clearest intent signal. Autocapture would only see a generic
  // click on a "Copy" button; a named event carrying the command is worth more.
  function wireInstallCopy() {
    document.querySelectorAll("[data-copy]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var target = document.querySelector(btn.getAttribute("data-copy"));
        mixpanel.track("install_command_copied", {
          command: target ? target.textContent.trim() : null,
          page: location.pathname,
        });
      });
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wireInstallCopy);
  } else {
    wireInstallCopy();
  }
})();
