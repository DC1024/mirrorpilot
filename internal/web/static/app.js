// The panel's entire client-side behaviour.
//
// It is a separate file rather than an inline <script> so the
// Content-Security-Policy can omit 'unsafe-inline'. Everything here is
// progressive enhancement: with JavaScript off the panel still authenticates,
// saves preferences, and changes the password — the pickers just require Enter
// instead of submitting themselves.
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
})();
