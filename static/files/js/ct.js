// ct.js — the small amount of client behaviour htmx and Alpine do not cover.
// Loaded before Alpine so the idiomorph guards are installed before any morph
// can run.
(function () {
  "use strict";

  // htmx's two eval paths, both off. allowEval governs hx-on:: and the js:
  // prefixes on hx-vals/hx-vars/hx-headers; none of them are used, and turning
  // it off means unsafe-eval is not needed on htmx's account.
  //
  // includeIndicatorStyles is not about eval: htmx injects a <style> element at
  // boot for .htmx-indicator, which style-src 'self' would refuse. Nothing here
  // defines or uses that class.
  //
  // Set before htmx boots — this file loads before it in page.html.
  if (window.htmx) {
    window.htmx.config.allowEval = false;
    window.htmx.config.includeIndicatorStyles = false;
  }

  // --- idiomorph guards -----------------------------------------------------
  //
  // A morph reverts attributes to whatever the server rendered, including ones
  // Alpine wrote. Alpine does not repair them: its effects re-run when the
  // reactive data changes, and a morph does not change the data. Elements match
  // by id, so _x_dataStack survives — it is only the DOM Alpine wrote that needs
  // protecting.
  //
  // The rule is derived from the bindings on the node rather than enumerated.
  // An enumerated list covered style and aria-expanded on x-show elements only,
  // which missed every other Alpine-written attribute on the page.
  if (window.Idiomorph) {
    var cb = Idiomorph.defaults.callbacks;

    cb.beforeAttributeUpdated = function (attr, node, mutationType) {
      if (!node.hasAttribute) return true;
      // Alpine strips x-cloak exactly once, at init. Re-adding it from the
      // server markup hides the element behind [x-cloak]{display:none} with
      // nothing to ever remove it again.
      if (attr === "x-cloak") return false;
      // x-show owns inline display.
      if (attr === "style" && node.hasAttribute("x-show")) return false;
      // Anything Alpine binds is Alpine's to write, not the morph's. This is
      // what covers :class and :aria-expanded on elements that have no x-show.
      if (node.hasAttribute(":" + attr) || node.hasAttribute("x-bind:" + attr)) {
        return false;
      }
      return true;
    };

    cb.beforeNodeMorphed = function (oldNode, newNode) {
      // An explicit opt-out for subtrees the client owns outright: a playing
      // <video>, and any panel whose content is fetched with hx-get — the
      // server markup for those is a placeholder, so morphing reverts the
      // fetched content back to it.
      if (oldNode.hasAttribute && oldNode.hasAttribute("data-preserve")) {
        return false;
      }
      return true;
    };
  }

  // --- download tree state --------------------------------------------------
  //
  // #downloads is morphed and every <li> carries a stable server-rendered id, so
  // a folder now stays open across a swap on its own: idiomorph matches the
  // element by id and Alpine's state survives in place. That covers an
  // EventSource reconnect too, which re-fires the hx-trigger and is therefore
  // just another swap.
  //
  // What a morph cannot help with is a full page reload: the document is rebuilt
  // and Alpine re-initialises from the server markup. That is the only reason
  // this survives.
  //
  // One key holding the ids whose state DIFFERS from the server-rendered
  // default, rather than one key per directory. The per-directory scheme wrote a
  // key for every folder ever seen and pruned none of them, so it grew without
  // bound; it also could not represent "top-level folder the user closed"
  // without storing a value for every folder.
  //
  // The id comes from data-id rather than being interpolated into x-data:
  // Alpine leaves _x_marker set on an initialised element, so changing an
  // x-data expression in place never re-initialises and breaks permanently.
  var TREE_KEY = "ct.tree.open";

  function readDeviations() {
    try {
      var raw = localStorage.getItem(TREE_KEY);
      var set = {}, ids = raw ? raw.split(",") : [];
      for (var i = 0; i < ids.length; i++) if (ids[i]) set[ids[i]] = true;
      return set;
    } catch (e) {
      return {}; // private mode, quota, or storage disabled
    }
  }

  function writeDeviations(set) {
    var ids = [];
    for (var k in set) {
      if (Object.prototype.hasOwnProperty.call(set, k)) ids.push(k);
    }
    try {
      localStorage.setItem(TREE_KEY, ids.join(","));
    } catch (e) { /* non-fatal: the tree just will not remember */ }
  }

  // Bounds the key to directories that still exist. Pruning against the
  // *rendered* tree means a folder hidden behind files.Limit loses its stored
  // state, which is the accepted cost of not growing forever.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (!e.target || e.target.id !== "downloads") return;
    var live = {}, els = e.target.querySelectorAll("[data-id]");
    for (var i = 0; i < els.length; i++) live[els[i].dataset.id] = true;
    var set = readDeviations(), kept = {};
    for (var k in set) {
      if (Object.prototype.hasOwnProperty.call(set, k) && live[k]) kept[k] = true;
    }
    writeDeviations(kept);
  });

  // confirmable is the two-step delete, shared by both tree components. It
  // disarms after a pause so a half-pressed delete does not sit armed
  // indefinitely, and the pending timer is cancelled on re-arm: without that,
  // two clicks less than the timeout apart let the first click's timer disarm
  // the second click's confirmation.
  //
  // The base defines no init, so a caller's init cannot be clobbered by the
  // merge below. Explicit copy rather than Object.assign: this file is ES5.
  function confirmable(extra) {
    var o = {
      confirm: false,
      _confirmTimer: null,
      ask: function () {
        if (this._confirmTimer) clearTimeout(this._confirmTimer);
        this.confirm = true;
        var self = this;
        this._confirmTimer = setTimeout(function () {
          self.confirm = false;
          self._confirmTimer = null;
        }, 3000);
      },
    };
    for (var k in extra) {
      if (Object.prototype.hasOwnProperty.call(extra, k)) o[k] = extra[k];
    }
    return o;
  }

  window.treeNode = function () {
    return confirmable({
      open: false,
      init: function () {
        var id = this.$el.dataset.id;
        // Whether an entry is top level is server-rendered (data-top), not
        // derived from how deeply the markup happens to be nested. Structure-
        // derived state breaks silently the moment the structure changes, which
        // is the same reason node paths are computed server-side.
        var dflt = this.$el.dataset.top === "1";
        this.open = readDeviations()[id] ? !dflt : dflt;
        this.$watch("open", function (v) {
          var set = readDeviations();
          if (v === dflt) delete set[id]; else set[id] = true;
          writeDeviations(set);
        });
      },
    });
  };

  // torrentRow exists only because its click handler cannot be an attribute
  // expression: the CSP evaluator parses expressions, not statements, and
  // `files = !files; if (files) $dispatch('load-files')` has both a sequence and
  // an if. Every other binding in the templates parses unchanged.
  //
  // The dispatch has to happen here rather than in the template for the same
  // reason, and it still bubbles from the button to the hx-trigger
  // "load-files from:closest article" on the panel.
  window.torrentRow = function () {
    return {
      files: false,
      toggleFiles: function () {
        this.files = !this.files;
        if (this.files) this.$dispatch("load-files");
      },
    };
  };

  window.treeLeaf = function () {
    return confirmable({
      // No `open` here, and no `preview` in treeNode: a directory is never
      // previewable — the view model clears Preview for one — so the preview
      // button never renders inside a treeNode.
      preview: false,
    });
  };

  // --- upload progress ------------------------------------------------------
  //
  // htmx emits htmx:xhr:progress only for a real multipart request, which is
  // why the upload form uses hx-encoding rather than reading the file in JS and
  // POSTing raw bytes — that path cannot report progress at all.
  document.body.addEventListener("htmx:xhr:progress", function (e) {
    // Scoped to the upload form, like its afterRequest sibling below. Unscoped,
    // any htmx request with a computable length drove the upload bar.
    if (!e.target || e.target.id !== "upload-form") return;
    var bar = document.getElementById("upload-progress");
    if (!bar || !e.detail.lengthComputable) return;
    bar.hidden = false;
    bar.value = (e.detail.loaded / e.detail.total) * 100;
  });

  // Moved out of the templates so no markup carries executable script: htmx
  // compiles hx-on:: with new Function and a bare onchange is inline script, and
  // both need script-src 'unsafe-inline' — the directive that actually stops an
  // injected script from running.
  //
  // requestSubmit rather than submit: it runs validation and fires the submit
  // event, which is what htmx listens for.
  document.body.addEventListener("change", function (e) {
    if (!e.target || e.target.id !== "torrent-file") return;
    if (e.target.form) e.target.form.requestSubmit();
  });

  document.body.addEventListener("htmx:afterRequest", function (e) {
    if (e.target && e.target.id === "omni-form" && e.detail && e.detail.successful) {
      e.target.reset();
    }
    if (!e.target || e.target.id !== "upload-form") return;
    var bar = document.getElementById("upload-progress");
    if (bar) { bar.hidden = true; bar.value = 0; }
    var input = document.getElementById("torrent-file");
    // Reset so re-picking the same file fires change again.
    if (input) input.value = "";
  });

  // htmx does not swap a non-2xx response and reports nothing at all on a
  // transport failure, so without these a failed fragment fetch left its
  // placeholder on screen forever with no trace anywhere. The placeholder
  // deliberately stays: both fragment panels retry on their own — #downloads on
  // the next ping, the file panel on the next expand — so what was wrong was the
  // silence, not the stale text.
  //
  // This also surfaces a rejected cross-origin mutation, which gets a hard 403
  // from requireSameOrigin rather than a fragment.
  document.body.addEventListener("htmx:responseError", function (e) {
    var status = e.detail && e.detail.xhr ? e.detail.xhr.status : 0;
    showError("Request failed (" + status + ").");
  });
  document.body.addEventListener("htmx:sendError", function () {
    showError("Could not reach the server.");
  });

  // --- drag and drop .torrent files ----------------------------------------
  //
  // Files are handed to the upload form's file input and submitted through
  // htmx, so dropping and clicking take exactly the same path — including
  // progress reporting.
  var dragDepth = 0;

  function setDropVisible(on) {
    document.body.classList.toggle("dropping", on);
  }

  document.addEventListener("dragenter", function (e) {
    if (!hasFiles(e)) return;
    dragDepth++;
    setDropVisible(true);
  });

  document.addEventListener("dragover", function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault(); // required, or the browser opens the file instead
    e.dataTransfer.dropEffect = "copy";
  });

  // dragleave fires for every child element, so a plain hide flickers; count
  // enter/leave pairs instead.
  document.addEventListener("dragleave", function (e) {
    if (!hasFiles(e)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) setDropVisible(false);
  });

  document.addEventListener("drop", function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth = 0;
    setDropVisible(false);

    var input = document.getElementById("torrent-file");
    var form = document.getElementById("upload-form");
    if (!input || !form) return;

    var accepted = new DataTransfer();
    for (var i = 0; i < e.dataTransfer.files.length; i++) {
      var f = e.dataTransfer.files[i];
      if (/\.torrent$/i.test(f.name)) accepted.items.add(f);
    }
    if (!accepted.files.length) {
      showError("Only .torrent files can be dropped here.");
      return;
    }
    input.files = accepted.files;
    form.requestSubmit();
  });

  function hasFiles(e) {
    return e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], "Files") !== -1;
  }

  // Built as nodes rather than as an HTML string. This is the only status the
  // client raises on its own — every other one arrives as an api-error fragment
  // from the server — and assembling markup here put a copy of the template
  // set's class contract in a file no template test can see.
  function showError(msg) {
    var el = document.getElementById("omni-status");
    if (!el) return;
    var p = document.createElement("p");
    p.className = "err-msg";
    p.textContent = msg;
    el.replaceChildren(p);
  }

  // --- spacebar toggles the first on-screen media ---------------------------
  // Ported from run.js. Survives swaps because it is delegated from document.
  document.addEventListener("keydown", function (e) {
    if (e.key !== " ") return;
    var el = document.activeElement;
    if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" ||
               el.tagName === "BUTTON" || el.isContentEditable)) {
      return;
    }
    var height = window.innerHeight || document.documentElement.clientHeight;
    var medias = document.querySelectorAll("video,audio");
    for (var i = 0; i < medias.length; i++) {
      var m = medias[i];
      var rect = m.getBoundingClientRect();
      var inView = (rect.top >= 0 && rect.top <= height) ||
                   (rect.bottom >= 0 && rect.bottom <= height);
      if (!inView) continue;
      if (m.paused) {
        // play() rejects when autoplay policy blocks it or the source is
        // missing. Unhandled, that is an uncaught rejection in the console on
        // every spacebar press against a blocked element.
        var played = m.play();
        if (played && played.catch) played.catch(function () {});
      } else {
        m.pause();
      }
      e.preventDefault();
      break;
    }
  });

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

  // The stream deliberately stays open in a background tab: htmx owns the
  // EventSource, so closing it from here means driving unexported internals,
  // whose failure mode is a permanently dead UI. See static/CLAUDE.md.
})();
