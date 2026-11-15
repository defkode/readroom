// Parts of a page that fold away (<details data-fold="name">): the categories of
// the feed list, the AI summary of an article. Which ones are folded is kept in
// this browser only (localStorage), like the display preferences. Loaded right
// after each such part, so a folded one is never painted open.
(function () {
  // A page may carry several such parts, and this script after each.
  if (window.readroomFolds) return window.readroomFolds.apply();
  const KEY = 'readroom:folded';
  const load = () => {
    try { return new Set(JSON.parse(localStorage.getItem(KEY) || '[]')); } catch (e) { return new Set(); }
  };
  const apply = () => {
    const folded = load();
    document.querySelectorAll('details[data-fold]').forEach((d) => { d.open = !folded.has(d.dataset.fold); });
  };
  apply();

  // "toggle" does not bubble, hence the capture. It also follows apply() itself, with nothing new to save.
  document.addEventListener('toggle', (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement) || !d.dataset.fold) return;
    const folded = load();
    if (folded.has(d.dataset.fold) === !d.open) return;
    if (d.open) folded.delete(d.dataset.fold); else folded.add(d.dataset.fold);
    try { localStorage.setItem(KEY, JSON.stringify([...folded])); } catch (e) { /* private mode: until the next page */ }
    apply(); // the Feeds page carries the feed list twice
  }, true);
  // Folded in another tab of the same browser: follow.
  addEventListener('storage', (e) => { if (e.key === KEY) apply(); });

  window.readroomFolds = { apply };
})();
