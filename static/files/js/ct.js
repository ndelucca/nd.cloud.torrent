// ct.js — the small amount of client behaviour htmx and Alpine do not cover.
// Loaded before Alpine so the idiomorph guards are installed before any morph
// can run.
(function () {
  "use strict";

  // --- idiomorph guards -----------------------------------------------------
  //
  // Verified in Chromium 150 (htmx 2.0.10 + idiomorph 0.7.4): a morph reverts
  // attributes to whatever the server rendered, including ones Alpine wrote
  // itself. x-show sets style="display:none"; after a morph that style is gone
  // and the element is visible again. Alpine does not repair it, because its
  // effects only re-run when the reactive *data* changes — and the morph did
  // not change the data. A collapsed panel visibly pops open once per second.
  //
  // Elements are matched by id, so Alpine's own state (_x_dataStack) survives.
  // It is only the DOM Alpine wrote that needs protecting.
  if (window.Idiomorph) {
    var cb = Idiomorph.defaults.callbacks;

    cb.beforeAttributeUpdated = function (attr, node, mutationType) {
      // Never let a morph touch inline style or aria-expanded on an element
      // whose visibility Alpine owns.
      if ((attr === "style" || attr === "aria-expanded") &&
          node.hasAttribute && node.hasAttribute("x-show")) {
        return false;
      }
      return true;
    };

    cb.beforeNodeMorphed = function (oldNode, newNode) {
      // An explicit opt-out for islands that must survive untouched, e.g. a
      // playing <video>.
      if (oldNode instanceof HTMLElement && oldNode.hasAttribute("data-preserve")) {
        return false;
      }
      return true;
    };
  }

  // --- connection indicator -------------------------------------------------
  function setConn(state) {
    var el = document.getElementById("connection");
    if (!el) return;
    el.dataset.state = state;
    var label = el.querySelector(".label");
    if (label) label.textContent = state;
  }

  document.body.addEventListener("htmx:sseOpen", function () { setConn("live"); });
  document.body.addEventListener("htmx:sseError", function () { setConn("offline"); });
  document.body.addEventListener("htmx:sseClose", function () { setConn("offline"); });

  // --- background tabs ------------------------------------------------------
  //
  // EventSource is NOT throttled in a hidden tab, so a backgrounded window
  // streams at full rate indefinitely. Without TLS this is HTTP/1.1, where the
  // browser allows only ~6 connections per origin — a few pinned tabs plus a
  // video preview and a zip download is enough to starve the stream.
  //
  // htmx owns the EventSource, so the cheapest correct lever is to let it
  // reconnect: closing on hide frees the socket, and htmx re-establishes it
  // when the element is processed again on show.
  var body = document.body;
  document.addEventListener("visibilitychange", function () {
    var internal = body["htmx-internal-data"];
    var src = internal && internal.sseEventSource;
    if (document.hidden) {
      if (src && src.readyState !== EventSource.CLOSED) src.close();
      setConn("paused");
    } else if (src && src.readyState === EventSource.CLOSED) {
      htmx.trigger(body, "htmx:abort"); // drop stale state
      htmx.process(body);               // re-register sse-connect
    }
  });
})();
