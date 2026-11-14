(() => {
  const mismatchText = 'Los correos electrónicos no coinciden.';

  function currentForm() {
    return document.querySelector('#customer-content form');
  }

  function normalizeEmail(raw) {
    const value = raw.trim();
    const at = value.lastIndexOf('@');
    return value.slice(0, at + 1) + value.slice(at + 1).toLowerCase();
  }

  function reconcileEmails(edited) {
    const form = currentForm();
    if (!form) return;
    const email = form.elements.namedItem('email');
    const confirmation = form.elements.namedItem('email_confirm');
    const status = form.querySelector('#save-status-email_confirm');

    confirmation.setCustomValidity('');
    let syntaxError = status.querySelector('[data-confirm-error="syntax"]');
    if (edited === confirmation && syntaxError) {
      syntaxError.remove();
      syntaxError = null;
    }

    const mismatch = email.value.trim() !== '' && confirmation.value.trim() !== '' &&
      email.validity.valid && confirmation.validity.valid &&
      normalizeEmail(email.value) !== normalizeEmail(confirmation.value);
    const mismatchError = status.querySelector('[data-confirm-error="mismatch"]');
    if (mismatch) {
      confirmation.setCustomValidity(mismatchText);
      if (!syntaxError && !mismatchError) {
        const message = document.createElement('p');
        message.setAttribute('role', 'alert');
        message.dataset.confirmError = 'mismatch';
        message.textContent = mismatchText;
        status.append(message);
      }
    } else {
      mismatchError?.remove();
    }
    if (mismatch || syntaxError) {
      confirmation.setAttribute('aria-invalid', 'true');
    } else {
      confirmation.removeAttribute('aria-invalid');
    }
  }

  function isDraft(control) {
    return typeof control?.matches === 'function' && control.matches('input[hx-params], select[hx-params]');
  }

  function activeDraft(control) {
    return isDraft(control) && control.isConnected && control.form === currentForm();
  }

  function draftTargets(control) {
    if (control?.id === 'date_of_birth_draft') {
      return Array.from(control.form?.querySelectorAll('[data-dob-part]') ?? []);
    }
    return control ? [control] : [];
  }

  function clearDraftInvalid(control) {
    for (const target of draftTargets(control)) {
      target.removeAttribute('aria-invalid');
    }
  }

  function syncDateDraft(mode) {
    const form = currentForm();
    const draft = form?.querySelector('#date_of_birth_draft');
    const dayEl = form?.querySelector('#dob_day');
    const monthEl = form?.querySelector('#dob_month');
    const yearEl = form?.querySelector('#dob_year');
    if (!draft || !dayEl || !monthEl || !yearEl || !activeDraft(draft)) return;

    const day = dayEl.value.trim();
    const month = monthEl.value.trim();
    const year = yearEl.value.trim();

    let next = null;
    if (day === '' && month === '' && year === '') {
      if (mode !== 'input') next = '';
    } else if (/^\d{1,2}$/.test(day) && Number(day) >= 1 && Number(day) <= 31 &&
        /^(0[1-9]|1[0-2])$/.test(month) && /^\d{4}$/.test(year)) {
      next = `${year}-${month}-${day.padStart(2, '0')}`;
    } else if (mode === 'leave' || (mode === 'change' && day !== '' && month !== '' && year !== '')) {
      next = 'invalid';
    }

    if (next === null || draft.value === next) return;
    draft.value = next;
    draft.dispatchEvent(new Event('change'));
  }

  // Generic save errors replace validation feedback without marking the field invalid.
  function reconcileDraftValidity(control, status) {
    const invalid = Boolean(status.querySelector('[data-validation="invalid"]'));
    for (const target of draftTargets(control)) {
      if (invalid) {
        target.setAttribute('aria-invalid', 'true');
      } else {
        target.removeAttribute('aria-invalid');
      }
    }
  }

  function ignoreDetachedDraft(event) {
    if (!activeDraft(event.currentTarget)) event.preventDefault();
  }

  document.addEventListener('input', (event) => {
    const control = event.target;
    if ((control.id === 'email' || control.id === 'email_confirm') && control.form === currentForm()) {
      reconcileEmails(control);
    }
    if (control.matches?.('[data-dob-part]') && control.form === currentForm()) {
      syncDateDraft('input');
    }
  });

  document.addEventListener('change', (event) => {
    const control = event.target;
    if (control.matches?.('[data-dob-part]') && control.form === currentForm()) {
      syncDateDraft('change');
    }
  });

  document.addEventListener('focusout', (event) => {
    const fieldset = event.target?.closest?.('#date_of_birth_group');
    if (!fieldset || !currentForm()?.contains(fieldset)) return;
    if (!fieldset.contains(event.relatedTarget)) {
      syncDateDraft('leave');
    }
  });

  document.addEventListener('htmx:beforeRequest', (event) => {
    const source = event.detail.requestConfig.elt;
    if (activeDraft(source)) {
      // Listen on the source itself: a detached draft cannot bubble to this document.
      source.addEventListener('htmx:beforeOnLoad', ignoreDetachedDraft);
    }
  });

  document.addEventListener('htmx:afterSwap', (event) => {
    const source = event.detail.requestConfig?.elt;
    if (isDraft(source)) {
      if (!activeDraft(source)) return;
      const status = document.getElementById(`save-status-${source.name}`);
      if (event.detail.elt !== status) return;
      reconcileDraftValidity(source, status);
      return;
    }
    if (!source?.matches('form[hx-target="#customer-content"]')) return;
    const form = currentForm();
    if (!form) return;
    reconcileEmails();
    if (event.detail.xhr.status === 422) {
      form.querySelector('[aria-invalid="true"], :invalid')?.focus();
    }
  });

  document.addEventListener('htmx:afterRequest', (event) => {
    if (event.detail.xhr.status !== 0) return;
    const source = event.detail.requestConfig?.elt;
    const form = currentForm();
    const draft = activeDraft(source);
    const target = draft
      ? document.getElementById(`save-status-${source.name}`)
      : source === form ? form.querySelector('#form-error') : null;
    if (!target) return;
    const message = document.createElement('p');
    message.setAttribute('role', 'alert');
    message.textContent = 'No se guardó. Verifica tu conexión e intenta de nuevo.';
    target.replaceChildren(message);
    if (draft) clearDraftInvalid(source);
  });

  reconcileEmails();
})();
