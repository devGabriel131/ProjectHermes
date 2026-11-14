const createForm = document.querySelector("#create-link");
const links = document.querySelector("#generated-links");
const status = document.querySelector("#link-status");
const refreshError = document.querySelector("#link-refresh-error");
const toast = document.querySelector("#copy-toast");
let requestSequence = 0;
let clipboardError = null;
let openDetails = new Set();
let renameDrafts = new Map();
let focusedField = null;

// Saved rows render relative paths; show the absolute URL staff will share. Then put back
// what a refresh replaced: open rows, a name being typed, and its caret.
function syncLinkRows() {
  for (const link of links.querySelectorAll("a.management-link-url")) {
    link.textContent = link.href;
  }
  for (const id of openDetails) {
    const details = document.getElementById(id);
    if (details) details.open = true;
  }
  for (const [id, value] of renameDrafts) {
    const input = document.getElementById(id);
    if (input) input.value = value;
  }
  const field = focusedField && document.getElementById(focusedField.id);
  if (field) {
    field.focus({ preventScroll: true });
    field.setSelectionRange(focusedField.start, focusedField.end);
  }
}
syncLinkRows();
links.addEventListener("htmx:beforeSwap", () => {
  openDetails = new Set([...links.querySelectorAll("details[open]")].map((d) => d.id));
  renameDrafts = new Map(
    [...links.querySelectorAll("details.management-link-rename[open] input")].map((i) => [i.id, i.value]),
  );
  const active = document.activeElement;
  focusedField = active instanceof HTMLInputElement && links.contains(active)
    ? { id: active.id, start: active.selectionStart, end: active.selectionEnd }
    : null;
});
links.addEventListener("htmx:afterSwap", syncLinkRows);

// The toast's CSS animation rises from the bottom, holds, then fades; its end
// empties the live region.
toast.addEventListener("animationend", () => { toast.textContent = ""; });

function showCopyError(message) {
  status.textContent = "";
  clipboardError = document.createTextNode(message);
  status.append(clipboardError);
}

function confirmCopy() {
  clipboardError?.remove();
  clipboardError = null;
  toast.textContent = "Link has been copied.";
  for (const animation of toast.getAnimations()) animation.currentTime = 0;
}

function startAction() {
  toast.textContent = "";
  return ++requestSequence;
}

links.addEventListener("click", async ({ target }) => {
  const button = target.closest("[data-copy-link]");
  if (!button) return;

  const sequence = startAction();
  const url = button.closest("li.management-link")?.querySelector("a.management-link-url")?.href;
  let copied = false;
  if (url) {
    try {
      await navigator.clipboard.writeText(url);
      copied = true;
    } catch {
      copied = false;
    }
  }
  if (sequence === requestSequence) {
    if (copied) confirmCopy();
    else showCopyError("Clipboard unavailable; select the link to copy it manually.");
  }
});

document.body.addEventListener("htmx:beforeRequest", ({ target }) => {
  if (target === links) return;
  startAction();
  if (target === createForm) {
    status.textContent = "Creating link…";
  }
});

document.body.addEventListener("htmx:afterRequest", ({ target, detail }) => {
  if (target === links) {
    refreshError.textContent = detail.successful
      ? ""
      : detail.xhr.responseText || "Could not refresh links. Reload to try again.";
    return;
  }
  if (!detail.successful) {
    status.textContent = detail.xhr.responseText || "Request failed. Please try again.";
    return;
  }
  const rename = target.closest("details.management-link-rename");
  if (rename) rename.open = false;
});
