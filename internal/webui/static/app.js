/* MCPaw admin console — the command palette.
 *
 * This is the only script the interface ships. It is served from the same
 * origin so the `script-src 'self'` Content-Security-Policy holds with no
 * inline handlers and no nonce required on the page.
 *
 * The palette is a real dialog, not a div that looks like one: it traps focus,
 * closes on Escape and on backdrop click, moves the active row with the arrow
 * keys, opens with Enter, and locks body scroll while it is open. Shipping the
 * pill without the keyboard model is the anti-pattern this avoids.
 */
(function () {
  "use strict";

  var root = document.getElementById("cmdk");
  var pill = document.getElementById("searchpill");
  if (!root || !pill) {
    return;
  }

  var input = root.querySelector("[data-cmdk-input]");
  var list = root.querySelector("[data-cmdk-list]");
  var panel = root.querySelector("[data-cmdk-panel]");
  var lastFocused = null;

  // Commands are rendered into the DOM by the server, which keeps this script
  // free of any knowledge about routes or instance names.
  var items = Array.prototype.slice.call(root.querySelectorAll("[data-cmdk-item]"));
  var groups = Array.prototype.slice.call(root.querySelectorAll("[data-cmdk-group]"));
  var empty = root.querySelector("[data-cmdk-empty]");
  var visible = items.slice();
  var activeIndex = 0;

  function isOpen() {
    return root.classList.contains("is-open");
  }

  function setActive(index) {
    if (visible.length === 0) {
      activeIndex = 0;
      return;
    }
    // Wrap at both ends so the list is a ring, which is what a keyboard user
    // expects from a palette.
    activeIndex = (index + visible.length) % visible.length;
    items.forEach(function (item) {
      item.classList.remove("is-active");
      item.removeAttribute("aria-selected");
    });
    var active = visible[activeIndex];
    active.classList.add("is-active");
    active.setAttribute("aria-selected", "true");
    if (input) {
      input.setAttribute("aria-activedescendant", active.id);
    }
    active.scrollIntoView({ block: "nearest" });
  }

  function filter(query) {
    var needle = query.trim().toLowerCase();
    visible = items.filter(function (item) {
      var haystack = (item.getAttribute("data-search") || item.textContent || "").toLowerCase();
      var match = needle === "" || haystack.indexOf(needle) !== -1;
      item.hidden = !match;
      return match;
    });

    // A group heading with nothing under it is noise.
    groups.forEach(function (group) {
      var name = group.getAttribute("data-cmdk-group");
      var any = visible.some(function (item) {
        return item.getAttribute("data-group") === name;
      });
      group.hidden = !any;
    });

    if (empty) {
      empty.hidden = visible.length !== 0;
    }
    setActive(0);
  }

  function open() {
    if (isOpen()) {
      return;
    }
    lastFocused = document.activeElement;
    root.classList.add("is-open");
    root.removeAttribute("aria-hidden");
    pill.setAttribute("aria-expanded", "true");
    document.body.style.overflow = "hidden";
    if (input) {
      input.value = "";
      filter("");
      input.focus();
    }
  }

  function close() {
    if (!isOpen()) {
      return;
    }
    root.classList.remove("is-open");
    root.setAttribute("aria-hidden", "true");
    pill.setAttribute("aria-expanded", "false");
    document.body.style.overflow = "";
    // Returning focus to where it came from is what makes the palette feel
    // like part of the page rather than a trapdoor.
    if (lastFocused && typeof lastFocused.focus === "function") {
      lastFocused.focus();
    }
  }

  pill.addEventListener("click", open);

  root.addEventListener("click", function (event) {
    if (event.target.hasAttribute("data-cmdk-close")) {
      close();
    }
  });

  document.addEventListener("keydown", function (event) {
    var isPaletteShortcut = (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k";
    if (isPaletteShortcut) {
      event.preventDefault();
      if (isOpen()) {
        close();
      } else {
        open();
      }
      return;
    }
    if (!isOpen()) {
      return;
    }
    switch (event.key) {
      case "Escape":
        event.preventDefault();
        close();
        break;
      case "ArrowDown":
        event.preventDefault();
        setActive(activeIndex + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        setActive(activeIndex - 1);
        break;
      case "Enter":
        if (visible.length > 0) {
          event.preventDefault();
          visible[activeIndex].click();
        }
        break;
      case "Tab":
        // Focus stays inside the panel for as long as it is open.
        if (panel && !panel.contains(event.target)) {
          event.preventDefault();
          if (input) {
            input.focus();
          }
        }
        break;
      default:
        break;
    }
  });

  if (input) {
    input.addEventListener("input", function () {
      filter(input.value);
    });
  }

  items.forEach(function (item, index) {
    item.addEventListener("mouseenter", function () {
      var position = visible.indexOf(item);
      if (position !== -1) {
        setActive(position);
      }
    });
    if (!item.id) {
      item.id = "cmdk-item-" + index;
    }
  });

  filter("");
})();
