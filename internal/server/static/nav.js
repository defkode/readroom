// Which categories of the feed list are folded is kept in this browser only
// (localStorage), like the display preferences. Loaded right after the list, so
// a folded category is never painted open.
(function () {
  // The Feeds page carries the list twice, and this script with it.
  if (window.readroomNav) return window.readroomNav.apply();
  const KEY = 'readroom:folded';
  const load = () => {
    try { return new Set(JSON.parse(localStorage.getItem(KEY) || '[]')); } catch (e) { return new Set(); }
  };
  const lists = () => document.querySelectorAll('.nav details[data-category]');
  const apply = () => {
    const folded = load();
    lists().forEach((d) => { d.open = !folded.has(d.dataset.category); });
  };
  apply();

  // "toggle" does not bubble, hence the capture. It also follows apply() itself, with nothing new to save.
  document.addEventListener('toggle', (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement) || !d.matches('.nav details[data-category]')) return;
    const folded = load();
    if (folded.has(d.dataset.category) === !d.open) return;
    if (d.open) folded.delete(d.dataset.category); else folded.add(d.dataset.category);
    try { localStorage.setItem(KEY, JSON.stringify([...folded])); } catch (e) { /* private mode: until the next page */ }
    apply(); // the Feeds page carries the list twice
  }, true);
  // Folded in another tab of the same browser: follow.
  addEventListener('storage', (e) => { if (e.key === KEY) apply(); });

  window.readroomNav = { apply };
})();
