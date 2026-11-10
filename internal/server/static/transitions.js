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

  addEventListener('pagereveal', (e) => {
    if (!e.viewTransition) return;
    let direction = '';
    try {
      const from = article(sessionStorage.getItem(fromKey));
      const to = article(location.pathname);
      if (to && !from) direction = 'forward';
      else if (from && !to) direction = 'back';
      else if (from && to && from !== to) {
        // Between two articles: their order in the list they were opened from.
        const ids = JSON.parse(sessionStorage.getItem('readroom-list')).items.map((item) => String(item.id));
        direction = ids.indexOf(to) < ids.indexOf(from) ? 'back' : 'forward';
      }
    } catch { /* storage unavailable or no list: plain cross-fade */ }
    if (direction) e.viewTransition.types.add(direction);
  });
})();
