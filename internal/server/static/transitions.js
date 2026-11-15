// Page changes are animated by the browser (cross-document view transitions,
// see app.css). This only tells it which way the reader is going, so the page
// slides the right way: forward into an article or on to the next one, back to
// the list or to the previous one. Loaded in <head>, before the first frame.
//
// The page being left notes its own path: the browser will not say where a
// navigation came from, because pages here send no referrer.
(function () {
  const article = (path) => ((path || '').match(/^\/entries\/(\d+)$/) || [])[1];
  const fromKey = 'readroom-from';

  addEventListener('pageswap', () => {
    try { sessionStorage.setItem(fromKey, location.pathname); } catch { /* plain cross-fade then */ }
  });

  // Which way a move between two paths goes: "forward", "back", or "" for a plain cross-fade.
  const direction = (fromPath, toPath) => {
    try {
      const from = article(fromPath);
      const to = article(toPath);
      if (to && !from) return 'forward';
      if (from && !to) return 'back';
      if (from && to && from !== to) {
        // Between two articles: their order in the list they were opened from.
        const ids = JSON.parse(sessionStorage.getItem('readroom-list')).items.map((item) => String(item.id));
        return ids.indexOf(to) < ids.indexOf(from) ? 'back' : 'forward';
      }
    } catch { /* storage unavailable or no list: plain cross-fade */ }
    return '';
  };
  // Pages swapped in place while the player is on (player.js) slide the same way.
  window.readroomDirection = direction;

  addEventListener('pagereveal', (e) => {
    if (!e.viewTransition) return;
    let way = '';
    try { way = direction(sessionStorage.getItem(fromKey), location.pathname); } catch { /* plain cross-fade */ }
    if (way) e.viewTransition.types.add(way);
  });
})();
