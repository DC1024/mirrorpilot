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
