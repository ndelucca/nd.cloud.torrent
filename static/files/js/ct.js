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

  // --- download tree state --------------------------------------------------
  //
  // The tree fragment is re-fetched and replaced wholesale whenever it changes,
  // so per-node state cannot live in the DOM. localStorage is what makes a
  // folder stay open across a refetch, a page reload, and an EventSource
  // reconnect — and reconnects do happen (laptop sleep, wifi), so without this
  // the tree would silently re-collapse and read as broken.
  //
  // The key comes from data-path rather than being interpolated into x-data:
  // Alpine leaves _x_marker set on an initialised element, so changing an
  // x-data expression in place never re-initialises and breaks permanently.
  var TREE_KEY = "ct.tree.";

  function readOpen(id, dflt) {
    try {
      var v = localStorage.getItem(TREE_KEY + id);
      return v === null ? dflt : v === "1";
    } catch (e) {
      return dflt; // private mode, quota, or storage disabled
    }
  }

  function writeOpen(id, open) {
    try {
      localStorage.setItem(TREE_KEY + id, open ? "1" : "0");
    } catch (e) { /* non-fatal: the tree just will not remember */ }
  }

  window.treeNode = function () {
    return {
      open: false,
      preview: false,
      confirm: false,
      init: function () {
        var id = this.$el.dataset.id;
        // Top-level entries default to open, deeper ones to closed, so a fresh
        // visit shows something useful without unfolding an entire tree.
        this.open = readOpen(id, this.$el.closest(".tree-list") === this.$el.parentElement &&
          this.$el.parentElement.parentElement.classList.contains("tree"));
        this.$watch("open", function (v) { writeOpen(id, v); });
      },
      ask: function () {
        var self = this;
        this.confirm = true;
        // Revert on its own, so a half-pressed delete does not sit armed.
        setTimeout(function () { self.confirm = false; }, 3000);
      },
    };
  };

  window.treeLeaf = function () {
    return {
      preview: false,
      confirm: false,
      ask: function () {
        var self = this;
        this.confirm = true;
        setTimeout(function () { self.confirm = false; }, 3000);
      },
    };
  };

  // --- upload progress ------------------------------------------------------
  //
  // htmx emits htmx:xhr:progress only for a real multipart request, which is
  // why the upload form uses hx-encoding rather than the FileReader + raw body
  // approach the AngularJS UI used (which could not report progress at all).
  document.body.addEventListener("htmx:xhr:progress", function (e) {
    var bar = document.getElementById("upload-progress");
    if (!bar || !e.detail.lengthComputable) return;
    bar.hidden = false;
    bar.value = (e.detail.loaded / e.detail.total) * 100;
  });

  document.body.addEventListener("htmx:afterRequest", function (e) {
    if (!e.target || e.target.id !== "upload-form") return;
    var bar = document.getElementById("upload-progress");
    if (bar) { bar.hidden = true; bar.value = 0; }
    var input = document.getElementById("torrent-file");
    // Reset so re-picking the same file fires change again.
    if (input) input.value = "";
  });

  // --- drag and drop .torrent files ----------------------------------------
  //
  // Ported from the AngularJS ondropfile directive. Files are handed to the
  // upload form's file input and submitted through htmx, so dropping and
  // clicking take exactly the same path — including progress reporting, which
  // the original drop handler did not have.
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
      showStatus('<p class="err-msg">Only .torrent files can be dropped here.</p>');
      return;
    }
    input.files = accepted.files;
    form.requestSubmit();
  });

  function hasFiles(e) {
    return e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], "Files") !== -1;
  }

  function showStatus(html) {
    var el = document.getElementById("omni-status");
    if (el) el.innerHTML = html;
  }

  // --- spacebar toggles the first on-screen media ---------------------------
  // Ported from run.js. Survives swaps because it is delegated from document.
  document.addEventListener("keydown", function (e) {
    if (e.key !== " " && e.keyCode !== 32) return;
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
      if (m.paused) { m.play(); } else { m.pause(); }
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
