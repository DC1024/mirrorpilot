// The panel's entire client-side behaviour.
//
// It is a separate file rather than an inline <script> so the
// Content-Security-Policy can omit 'unsafe-inline'. Everything here is
// progressive enhancement: with JavaScript off the panel still authenticates,
// saves preferences, changes the password, generates configuration and
// dispatches workflow runs. The pickers just require Enter instead of
// submitting themselves, and a generated file has to be selected by hand
// instead of copied with one click.
(function () {
  "use strict";

  // Pickers that act as soon as a choice is made.
  //
  // Change events only fire for a committed selection, so arrow-key browsing
  // through a <select> does not fire a request per option.
  document.addEventListener("change", function (event) {
    var target = event.target;
    if (!target.matches("[data-autosubmit]")) {
      return;
    }
    var form = target.form;
    if (!form) {
      return;
    }
    if (typeof form.requestSubmit === "function") {
      form.requestSubmit();
    } else {
      form.submit();
    }
  });

  // Forms whose consequences deserve a confirmation step.
  //
  // Intercepting submit rather than using a click handler means Enter-to-submit
  // is covered too.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!form.matches("[data-confirm]")) {
      return;
    }
    if (!window.confirm(form.getAttribute("data-confirm"))) {
      event.preventDefault();
    }
  });

  /*
    POLL_MS is how often the panel asks what is being measured.

    Under a second, because the quickest layer finishes in tens of milliseconds
    and a reader should see it land. The answer is a few hundred bytes, and
    nothing else is happening on this connection while a run is going.
  */
  var POLL_MS = 700;

  // Speed-test forms.
  //
  // A batch runs for minutes on the server and the page is blank the whole
  // time, so the browser's only cue is a spinning tab that says nothing. A
  // submit handler raises a covering panel and — because "working" and "stuck"
  // look identical from here — asks the server which layer each mirror is on
  // and how long it has been there. Most of a run is spent in the last layer,
  // waiting on somebody else's bandwidth, which is exactly the part that needs
  // to be visible.
  //
  // It does not preventDefault: without JavaScript the form still posts and
  // redirects exactly as before, and the panel is simply never raised.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!form.matches("[data-probe-busy]")) {
      return;
    }
    beginProbeWait(form);
  });

  function beginProbeWait(form) {
    var panel = document.getElementById("probe-busy");
    if (!panel) {
      return;
    }

    // One whole batch, one form. A double submit would otherwise restart the
    // clock and raise the panel twice.
    form.setAttribute("disabled", "");

    var started = Date.now();

    // A count-up rather than a progress bar: the total is not known ahead of
    // time, and a bar that fills to ninety percent and stops is a worse lie
    // than a clock. It stays visible under the per-mirror rows, because
    // "three mirrors are being measured" and "the whole run has been going for
    // two minutes" are different questions.
    var timer = document.getElementById("probe-busy-timer");
    var hint = timer ? timer.getAttribute("data-label") || "" : "";
    var tick = function () {
      if (!timer) {
        return;
      }
      timer.textContent = fill(hint, [secondsSince(started)]);
    };
    tick();

    var clock = window.setInterval(tick, 1000);
    var poll = 0;

    // The run outlives this page: the browser replaces the document as soon as
    // the response arrives. Polling stops on the way out rather than being
    // left to fail against a page that is no longer there.
    var stop = function () {
      window.clearInterval(clock);
      window.clearInterval(poll);
      window.removeEventListener("pagehide", stop);
    };
    window.addEventListener("pagehide", stop);

    panel.hidden = false;

    var ask = function () {
      fetchProgress(panel, started);
    };
    poll = window.setInterval(ask, POLL_MS);
    ask();
  }

  function secondsSince(started) {
    return Math.floor((Date.now() - started) / 1000);
  }

  /*
    fill replaces the numbered placeholders in a format string, in order.

    %s and %d alike. The strings come from the locale files, where the two
    languages are required to carry the same placeholders in the same order —
    the catalogue has a test for that — so a formatter that insisted on the
    type would be a second rule to keep in step with the first.
  */
  function fill(format, values) {
    var i = 0;
    return String(format || "").replace(/%[sd]/g, function () {
      return i < values.length ? String(values[i++]) : "";
    });
  }

  // fetchProgress asks the server what the run is doing, and draws the answer.
  function fetchProgress(panel, started) {
    window
      .fetch(panel.getAttribute("data-progress"), {
        headers: { Accept: "application/json" },
        credentials: "same-origin"
      })
      .then(function (response) {
        var type = response.headers.get("Content-Type") || "";
        if (!response.ok || type.indexOf("application/json") < 0) {
          // A session that expired mid-run comes back as the login page rather
          // than as an error. Drawing that as progress would be nonsense, so
          // the panel keeps its count-up and says nothing extra.
          return;
        }
        return response.json().then(function (state) {
          drawProgress(panel, state, started);
        });
      })
      .catch(function () {
        // One failed poll is not worth interrupting a run over; the next is
        // along shortly. If the panel is genuinely unreachable the count-up
        // stays, which is what it showed before any of this existed.
      });
  }

  function drawProgress(panel, state, started) {
    var summary = document.getElementById("probe-busy-progress");
    var list = document.getElementById("probe-busy-steps");
    if (!summary || !list) {
      return;
    }

    if (!state.active || !state.steps || !state.steps.length) {
      // Nothing registered: either the run has not reached the server yet, or
      // it is over and this page is about to be replaced by the results.
      summary.hidden = true;
      list.hidden = true;
      return;
    }

    // How far the server's clock had got when it answered. Each row's elapsed
    // time is measured on from there, so the seconds keep moving between polls
    // instead of freezing at whatever the server last reported.
    var drift = Date.now() - started - (state.elapsed_ms || 0);

    summary.textContent = fill(panel.getAttribute("data-progress-label"), [
      state.done || 0,
      state.total || 0,
      secondsSince(started)
    ]);
    summary.hidden = false;

    // Rebuilt wholesale. There are a handful of rows and reconciling them
    // would be more code than it saves.
    list.textContent = "";
    list.hidden = false;

    state.steps.forEach(function (step) {
      list.appendChild(stepRow(panel, step, drift));
    });
  }

  function stepRow(panel, step, drift) {
    var row = document.createElement("li");
    row.className = "probe-step" + (step.done ? " is-done" : "");

    var name = document.createElement("span");
    name.className = "probe-step-name";
    name.textContent = step.name || step.id;
    row.appendChild(name);

    // The layer in flight, with a clock of its own. A mirror that has been on
    // one layer for forty seconds is the single most useful thing this panel
    // can say — it is what a slow mirror looks like, as opposed to a run that
    // never started.
    if (step.layer) {
      var what = document.createElement("span");
      what.className = "probe-step-what";
      what.textContent = fill(panel.getAttribute("data-step-format"), [
        panel.getAttribute("data-step-" + step.layer) || step.layer,
        Math.max(0, Math.round((step.layer_ms + drift) / 1000))
      ]);
      row.appendChild(what);
    }

    if (step.trace && step.trace.length) {
      row.appendChild(traceChips(step.trace));
    }

    return row;
  }

  /*
    traceChips renders the layers that have already finished.

    Both halves of each chip are worded by the server: the units depend on the
    quantity, and a throughput printed in milliseconds looks entirely plausible
    and is wrong by three orders of magnitude. The layer's own explanation, if
    it had one, rides along as a tooltip — a failure's reason is worth having,
    and four of them inline would bury the numbers.
  */
  function traceChips(trace) {
    var line = document.createElement("span");
    line.className = "probe-step-trace";

    trace.forEach(function (entry) {
      var chip = document.createElement("span");
      chip.className = "pill probe-chip " + (entry.class || "pill-muted");
      chip.textContent = entry.label + " " + entry.value;
      if (entry.detail) {
        chip.title = entry.detail;
      }
      line.appendChild(chip);
    });

    return line;
  }

  // Copy buttons.
  //
  // The text is read out of the element the button names rather than out of a
  // data attribute, so the page carries one copy of a generated file instead of
  // two — and a document full of newlines and quotes does not have to survive
  // being written into an attribute.
  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy]");
    if (!button) {
      return;
    }

    var source = document.querySelector(button.getAttribute("data-copy"));
    if (!source) {
      return;
    }

    copyText(source.textContent).then(
      function () {
        announce(button, button.getAttribute("data-copy-done"));
      },
      function () {
        announce(button, button.getAttribute("data-copy-failed"));
      }
    );
  });

  /*
    copyText puts a string on the clipboard.

    execCommand first, and deliberately: the panel is normally reached over
    plain HTTP at a bare address, where navigator.clipboard does not exist
    because it requires a secure context. Trying the modern API first would
    mean the button silently did nothing for most visitors.
  */
  function copyText(text) {
    return new Promise(function (resolve, reject) {
      var area = document.createElement("textarea");
      area.className = "copyarea";
      area.value = text;
      area.setAttribute("readonly", "");
      area.setAttribute("aria-hidden", "true");
      document.body.appendChild(area);

      // select() alone does not reach the value on every mobile browser.
      area.select();
      area.setSelectionRange(0, text.length);

      var copied = false;
      try {
        copied = document.execCommand("copy");
      } catch (err) {
        copied = false;
      }
      document.body.removeChild(area);

      if (copied) {
        resolve();
        return;
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(resolve, reject);
        return;
      }
      reject(new Error("copy is not available in this browser"));
    });
  }

  // announce swaps a button's label for a moment, then puts it back.
  function announce(button, message) {
    if (!message) {
      return;
    }
    if (!button.hasAttribute("data-label")) {
      button.setAttribute("data-label", button.textContent);
    }

    button.textContent = message;
    window.setTimeout(function () {
      button.textContent = button.getAttribute("data-label");
    }, 1500);
  }
})();
