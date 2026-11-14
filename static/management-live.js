// The server sends a message whenever data shown on this page changes, and one
// per connection to catch up. Live regions re-fetch themselves on
// "management-changed", the same event htmx actions trigger after a change.
// Messages arriving within a second share one trailing refresh, so a burst of
// enrollments re-renders each region once rather than once per commit.
let due;

function listenForChanges() {
  const changes = new EventSource("/management/events");
  changes.onmessage = () => {
    due ??= setTimeout(() => {
      due = undefined;
      htmx.trigger(document.body, "management-changed");
    }, 1000);
  };
  changes.onerror = () => {
    // EventSource reconnects dropped streams itself but gives up on a failed
    // response, such as a deploy's 502 or a sign-in redirect. Refresh now,
    // which sends signed-out pages to sign in, and listen again shortly.
    if (changes.readyState !== EventSource.CLOSED) return;
    htmx.trigger(document.body, "management-changed");
    setTimeout(listenForChanges, 5000);
  };
}

if (window.htmx) listenForChanges();
