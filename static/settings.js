// Mirrors the lifetime slider into its <output>; the site CSP blocks inline handlers.
const lifetimeForm = document.querySelector(".management-settings-form");
lifetimeForm.addEventListener("input", () => {
  lifetimeForm.elements.lifetime_days.value = lifetimeForm.elements.lifetime.value;
});
