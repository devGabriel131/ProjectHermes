const studentDialog = document.querySelector("#student-dialog");
const studentRequests = new WeakMap();
let studentOpenerRowID;

document.addEventListener("click", (event) => {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  const row = event.target.closest(".student-table tr[data-student-id]");
  if (!row) return;
  const control = event.target.closest("a, button, input, select, textarea, label, summary, [contenteditable], [tabindex], [role='button']");
  if (control && row.contains(control)) return;
  if (window.getSelection()?.toString()) return;
  row.querySelector("[data-student-link]")?.click();
});

// The element a request reports its failure into: its field's, its billing block's, or the page's.
// The billing slot sits beside the block, so the block's live refresh never replaces it.
function requestError(request) {
  if (request.fieldID) return document.getElementById(request.fieldID)?.querySelector(".student-request-error");
  if (request.billingID) return document.getElementById(`${request.billingID}-error`);
  return document.querySelector("main.management-students > .student-request-error");
}

document.body.addEventListener("htmx:beforeRequest", ({ detail }) => {
  const source = detail.requestConfig?.elt ?? detail.elt;
  const field = source.closest(".student-field");
  // Billing actions only: the block's own live refresh fires on any change and must not clear a refusal.
  const billing = source.matches(".student-billing") ? null : source.closest(".student-billing");
  const link = source.closest("[data-student-link]");
  // A filter change or live refresh; tracked so a replaced request's error clears with the next.
  const roster = source.matches("#student-filter, #student-records");
  if (!field && !link && !roster && !billing) return;

  const request = {
    fieldID: field?.id,
    billingID: billing?.id,
    rowID: link?.closest("tr[data-student-id]")?.id,
    message: source.matches("form.student-field-editor")
      ? "Could not save this field. Please try again."
      : billing ? "Could not complete the billing request. Please try again."
      : roster ? "Could not load students. Please try again." : "Could not load student. Please try again.",
  };
  studentRequests.set(detail.xhr, request);
  const error = requestError(request);
  if (error) {
    error.hidden = true;
    error.textContent = "";
  }
});

document.body.addEventListener("htmx:beforeSwap", ({ detail }) => {
  if (!studentRequests.has(detail.xhr)) return;
  if (detail.xhr.status === 422 && detail.requestConfig.elt.matches("form.student-field-editor")) {
    detail.shouldSwap = true;
    detail.isError = false;
  } else if (detail.xhr.status >= 400) {
    detail.shouldSwap = false;
  }
});

document.body.addEventListener("htmx:afterSwap", (event) => {
  const { detail } = event;
  const request = studentRequests.get(detail.xhr);
  if (!request) return;

  if (studentDialog && event.target.id === "student-dialog-content" && detail.xhr.status >= 200 && detail.xhr.status < 300) {
    studentOpenerRowID = request.rowID;
    if (!studentDialog.open) studentDialog.showModal();
  }
  if (request.fieldID && event.target.id === request.fieldID) {
    const field = document.getElementById(request.fieldID);
    if (field?.closest("dialog") && !studentDialog?.open) return;
    field?.querySelector("input:not([type='hidden']), select, textarea, .student-edit-link")?.focus();
  }
});

studentDialog?.addEventListener("close", () => {
  document.getElementById(studentOpenerRowID)?.querySelector("[data-student-link]")?.focus();
});

document.body.addEventListener("htmx:afterRequest", ({ detail }) => {
  const request = studentRequests.get(detail.xhr);
  if (!request || detail.successful || detail.xhr.getResponseHeader("HX-Redirect")) return;
  const error = requestError(request);
  if (!error) return;
  const reply = detail.xhr.getResponseHeader("Content-Type")?.startsWith("text/plain") ? detail.xhr.responseText.trim() : "";
  error.textContent = request.billingID && reply ? reply : request.message;
  error.hidden = false;
});
