// The web app is a thin client of the same REST API that scripts use.
const api = async (method, path, body) => {
  const opts = { method, headers: {} };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch('/api/v1' + path, opts);
  if (res.status === 401) { location.href = '/login'; throw new Error('Signed out'); }
  const data = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) throw new Error((data && data.error) || `Request failed (${res.status})`);
  return data;
};

// A toast may carry one action, such as undoing what it reports; it then stays longer.
let toastTimer;
const toast = (msg, action) => {
  const el = document.getElementById('toast');
  el.textContent = msg;
  if (action) {
    const button = document.createElement('button');
    button.textContent = action.label;
    button.onclick = () => { el.classList.remove('show'); action.run().catch((err) => toast(err.message)); };
    el.append(button);
  }
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), action ? 8000 : 2600);
};

const busy = async (el, fn) => {
  el.disabled = true;
  try { return await fn(); } finally { el.disabled = false; }
};

const feedDialog = () => document.getElementById('feed-dialog');
const syncSince = (form) => { form.querySelector('.since').hidden = !form.preserve.checked; };

// Reflect the stored display preferences in the settings sheet.
const syncPrefs = () => {
  const prefs = readroomPrefs.get();
  document.querySelectorAll('#prefs-dialog [data-pref]').forEach((group) => {
    const name = group.dataset.pref;
    if (name === 'size') {
      group.querySelector('output').textContent = prefs.size + ' px';
      return;
    }
    group.querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', b.value === String(prefs[name])));
  });
};

const rowOf = (el) => document.querySelector(`#entries .entry[data-id="${el.dataset.id}"]`);
const closeMenu = (el) => el.closest('[popover]').hidePopover();

const setRead = async (row, read) => {
  await api('PATCH', `/entries/${row.dataset.id}`, { read });
  row.classList.toggle('read', read);
  refreshCounts();
};
const setStar = async (row, starred) => {
  await api('PATCH', `/entries/${row.dataset.id}`, { starred });
  row.classList.toggle('starred', starred);
  refreshCounts();
};

// Counts live in the navigation, the view tabs and the list menu; take them from a fresh copy of this page.
const refreshCounts = async () => {
  const res = await fetch(location.href);
  if (!res.ok) return;
  const fresh = new DOMParser().parseFromString(await res.text(), 'text/html');
  for (const part of ['.sidebar .nav', '.tabbar', '#page-menu', '.segmented']) {
    const from = fresh.querySelector(part), to = document.querySelector(part);
    if (from && to) to.replaceWith(from);
  }
};

// Marking many articles reloads the list; the offer to undo is carried across the reload.
const undoKey = 'readroom-undo';
const markRead = async (el, body) => {
  closeMenu(el);
  if (+el.dataset.feed) body.feed_id = +el.dataset.feed;
  if ('category' in el.dataset) body.category = el.dataset.category;
  const { marked, ids } = await api('POST', '/entries/mark-read', body);
  if (!marked) return toast('Nothing unread here');
  try { sessionStorage.setItem(undoKey, JSON.stringify({ ids, at: Date.now() })); } catch { /* no undo then */ }
  location.reload();
};

// Opening an article marks it read, and it is gone from Unread on the way back.
// The article notes that it was this visit that marked it; the next page that is
// not an article offers to take that back, for every article read on the way.
const openedKey = 'readroom-opened';
const opened = () => {
  try { return JSON.parse(sessionStorage.getItem(openedKey)) || []; } catch { return []; }
};
const setOpened = (ids) => {
  try {
    if (ids.length) sessionStorage.setItem(openedKey, JSON.stringify(ids));
    else sessionStorage.removeItem(openedKey);
  } catch { /* no undo then */ }
};
const forgetOpened = (id) => setOpened(opened().filter((other) => other !== +id));
const offerUnread = () => {
  const ids = opened();
  if (!ids.length) return;
  setOpened([]);
  toast(ids.length === 1 ? 'Marked as read' : `Marked ${ids.length} as read`, { label: 'Undo', run: async () => {
    await api('POST', '/entries/mark-unread', { ids });
    location.reload();
  } });
};
const firstRead = document.querySelector('.article[data-first-read]');
if (firstRead) setOpened([...new Set([...opened(), +firstRead.dataset.id])]);

try {
  const undo = JSON.parse(sessionStorage.getItem(undoKey));
  sessionStorage.removeItem(undoKey);
  if (document.querySelector('.article[data-id]')) { /* still reading: the list will offer it */ }
  else if (!(undo && Date.now() - undo.at < 5000)) offerUnread();
  else {
    setOpened([]); // one message at a time: the newer action is the one on offer
    if (undo.hidden) {
      toast('Hidden', { label: 'Undo', run: async () => {
        await api('PATCH', `/entries/${undo.hidden}`, { hidden: false });
        location.reload();
      } });
    } else {
      toast(`Marked ${undo.ids.length} as read`, { label: 'Undo', run: async () => {
        await api('POST', '/entries/mark-unread', { ids: undo.ids });
        location.reload();
      } });
    }
  }
} catch { /* storage unavailable */ }

// Ask for a local copy and wait for it: the copy is made in the background.
const keepCopy = async (id) => {
  await api('POST', `/entries/${id}/preserve`);
  toast('Saving a local copy…');
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 1500));
    const e = await api('GET', `/entries/${id}`);
    if (e.preserved) return true;
    if (e.preserve_error) { toast('Could not save: ' + e.preserve_error); return false; }
  }
  return false;
};

// Deleting a copy cannot be taken back if the original has since gone, so it asks first.
const dropCopy = async (id) => {
  if (!confirm('Remove the local copy of this article?\n\nThe text and images saved here are deleted. The article stays, as the feed sent it.')) return null;
  return api('DELETE', `/entries/${id}/preserve`); // the article as it is now
};

// The notice about feeds that stopped updating can be put away on this device;
// it comes back when the set of failing feeds changes.
const outageKey = 'readroom:outage';
const outage = document.getElementById('outage');
if (outage) {
  let dismissed = null;
  try { dismissed = localStorage.getItem(outageKey); } catch { /* always shown */ }
  outage.hidden = dismissed === outage.dataset.key;
}

// Hiding puts an article out of every list for good; the row goes at once and
// the message that follows can bring it back.
const hideRow = async (row) => {
  const id = row.dataset.id;
  await api('PATCH', `/entries/${id}`, { hidden: true });
  row.remove();
  if (!document.querySelector('#entries .entry')) return location.reload();
  refreshCounts();
  toast('Hidden', { label: 'Undo', run: async () => {
    await api('PATCH', `/entries/${id}`, { hidden: false });
    location.reload();
  } });
};

const actions = {
  async 'toggle-star'(el) {
    const on = !el.classList.contains('on');
    await api('PATCH', `/entries/${el.dataset.id}`, { starred: on });
    el.classList.toggle('on', on);
    el.setAttribute('aria-pressed', on);
  },
  async 'mark-unread'(el) {
    await api('PATCH', `/entries/${el.dataset.id}`, { read: false });
    forgetOpened(el.dataset.id);
    history.length > 1 ? history.back() : (location.href = '/');
  },
  async preserve(el) {
    if (await keepCopy(el.dataset.id)) location.reload();
  },
  async unpreserve(el) {
    if (await dropCopy(el.dataset.id)) location.reload();
  },
  async 'row-lists'(el) {
    const id = el.dataset.id;
    closeMenu(el);
    const { readlists = [] } = await api('GET', `/entries/${id}`);
    const dialog = document.getElementById('lists-dialog');
    dialog.querySelectorAll('#list-checks input').forEach((box) => {
      box.dataset.id = id;
      box.checked = readlists.includes(+box.dataset.list);
    });
    dialog.querySelector('[data-action="create-list"]').dataset.id = id;
    dialog.showModal();
  },
  // Keep local copies for the whole feed from now on, starting with this article.
  async 'preserve-feed'(el) {
    await api('PATCH', `/feeds/${el.dataset.feed}`, { preserve: true });
    if (await keepCopy(el.dataset.id)) location.reload();
  },
  // Not awaited: the copy takes a while and the shared menu must stay usable for other rows.
  'row-preserve'(el) {
    const row = rowOf(el);
    closeMenu(el);
    if (row.classList.contains('kept')) {
      return dropCopy(el.dataset.id).then((entry) => {
        if (!entry) return;
        row.classList.remove('kept');
        // Its thumbnail was one of the deleted images.
        const thumb = row.querySelector('.thumb');
        if (thumb) entry.image ? (thumb.src = entry.image) : thumb.remove();
        toast('Local copy removed');
      });
    }
    keepCopy(el.dataset.id).then((kept) => {
      if (!kept) return;
      row.classList.add('kept');
      toast('Saved locally');
    }).catch((err) => toast(err.message));
  },
  'mark-all-read'(el) { return markRead(el, {}); },
  'read-through'(el) {
    const body = { through: +el.dataset.id };
    if (el.dataset.order) body.order = el.dataset.order; // "below" in the order this list is shown in
    return markRead(el, body);
  },
  'row-hide'(el) {
    const row = rowOf(el);
    closeMenu(el);
    return hideRow(row);
  },
  async 'row-unhide'(el) {
    const row = rowOf(el);
    closeMenu(el);
    await api('PATCH', `/entries/${el.dataset.id}`, { hidden: false });
    row.remove();
    if (!document.querySelector('#entries .entry')) return location.reload();
    toast('Shown again');
  },
  // From the article: hide it and go back to the list, which offers the undo.
  async 'hide-entry'(el) {
    await api('PATCH', `/entries/${el.dataset.id}`, { hidden: true });
    try { sessionStorage.setItem(undoKey, JSON.stringify({ hidden: +el.dataset.id, at: Date.now() })); } catch { /* no undo then */ }
    location.href = document.querySelector('.topbar .back').href;
  },
  async 'unhide-entry'(el) {
    await api('PATCH', `/entries/${el.dataset.id}`, { hidden: false });
    location.reload();
  },
  'row-read'(el) {
    const row = rowOf(el);
    closeMenu(el);
    return setRead(row, !row.classList.contains('read'));
  },
  'row-star'(el) {
    const row = rowOf(el);
    closeMenu(el);
    return setStar(row, !row.classList.contains('starred'));
  },
  async 'load-more'(el) {
    const res = await fetch(el.dataset.url);
    const html = await res.text();
    el.closest('li').remove();
    document.getElementById('entries').insertAdjacentHTML('beforeend', html);
  },
  'open-dialog'(el) {
    const dialog = document.getElementById(el.dataset.dialog);
    if (dialog.id === 'prefs-dialog') syncPrefs();
    dialog.showModal();
  },
  'toggle-sidebar'() { readroomPrefs.set({ sidebar: readroomPrefs.get().sidebar === 'hidden' ? 'shown' : 'hidden' }); },
  'reset-prefs'() { readroomPrefs.reset(); syncPrefs(); },
  'close-dialog'(el) { el.closest('dialog').close(); },

  'edit-feed'(el) {
    const menu = el.closest('[popover]');
    if (menu) menu.hidePopover();
    const form = feedDialog().querySelector('form');
    const d = el.dataset;
    form.dataset.id = d.id;
    form.title.value = d.title;
    form.title.placeholder = d.feedTitle || 'Feed name';
    form.url.value = d.url;
    form.category.value = d.category;
    form.preserve.checked = d.preserve === 'true';
    form.since.value = d.since;
    form.ai.checked = d.ai === 'true';
    // A period set through the API may not be one of the offered choices.
    if (![...form.keep.options].some((o) => o.value === d.keep)) form.keep.add(new Option(`${d.keep} days`, d.keep));
    form.keep.value = d.keep;
    syncSince(form);
    feedDialog().showModal();
  },
  async 'refresh-feed'(el) {
    const feed = await api('POST', `/feeds/${el.closest('form').dataset.id}/refresh`);
    toast(`Refreshed · ${feed.total} articles`);
  },
  async 'retry-feed'(el) {
    await api('POST', `/feeds/${el.dataset.id}/refresh`); // a failure is reported by the caller
    location.reload();
  },
  'dismiss-outage'(el) {
    const notice = el.closest('.notice');
    try { localStorage.setItem(outageKey, notice.dataset.key); } catch { /* shown again next time */ }
    notice.hidden = true;
  },
  async 'delete-feed'(el) {
    const form = el.closest('form');
    if (!confirm(`Delete "${form.title.value || form.title.placeholder}" and all its articles?`)) return;
    await api('DELETE', `/feeds/${form.dataset.id}`);
    // Deleted from the feed's own list, there is nothing left here to show.
    new URLSearchParams(location.search).has('feed_id') ? (location.href = '/') : location.reload();
  },
  // Menus are shared popovers, placed under the button that opened them. A row's
  // button also hands the menu its article and that article's current state.
  'open-menu'(el) {
    const menu = document.getElementById(el.dataset.menu);
    if (el.dataset.id) {
      const row = el.closest('li');
      const label = (action, text) => { menu.querySelector(`[data-action="${action}"] span`).textContent = text; };
      menu.querySelectorAll('button').forEach((b) => { b.dataset.id = el.dataset.id; });
      label('row-read', row.classList.contains('read') ? 'Mark as unread' : 'Mark as read');
      label('row-star', row.classList.contains('starred') ? 'Remove star' : 'Star');
      const kept = row.classList.contains('kept');
      label('row-preserve', kept ? 'Remove local copy' : 'Keep a local copy');
    }
    menu.showPopover();
    const at = el.getBoundingClientRect();
    const below = at.bottom + 4 + menu.offsetHeight <= innerHeight - 8;
    menu.style.top = (below ? at.bottom + 4 : Math.max(8, at.top - 4 - menu.offsetHeight)) + 'px';
    menu.style.left = Math.max(8, at.right - menu.offsetWidth) + 'px';
  },
  async 'remove-from-list'(el) {
    closeMenu(el);
    await api('DELETE', `/readlists/${el.dataset.list}/entries/${el.dataset.id}`);
    rowOf(el).remove();
    // Keep the sidebar count in step; an emptied list reloads to show its empty state.
    if (!document.querySelector('#entries .entry')) return location.reload();
    const count = document.querySelector(`a[href="/?readlist_id=${el.dataset.list}"] b`);
    if (count) count.textContent = Math.max(0, count.textContent - 1);
    toast('Removed from readlist');
  },
  async 'delete-list'(el) {
    if (!confirm(`Delete readlist "${el.dataset.name}"? Its articles are kept.`)) return;
    await api('DELETE', `/readlists/${el.dataset.id}`);
    location.reload();
  },
};

const submits = {
  async 'add-feed'(form) {
    const feed = await api('POST', '/feeds', { url: form.url.value, category: form.category.value, unread: form.unread.value });
    // A feed that sends only the opening of each article: offer to fetch the rest.
    if (feed.teaser_only && !feed.preserve &&
        confirm(`"${feed.title}" sends only a teaser of each article.\n\nKeep local copies, so the full text and images are here?`)) {
      await api('PATCH', `/feeds/${feed.id}`, { preserve: true });
    }
    location.reload();
  },
  async 'save-feed'(form) {
    await api('PATCH', `/feeds/${form.dataset.id}`, {
      title: form.title.value,
      url: form.url.value,
      category: form.category.value,
      preserve: form.preserve.checked,
      preserve_since: form.since.value || null,
      ai_summary: form.ai.checked,
      keep_days: +form.keep.value,
    });
    location.reload();
  },
  async 'create-list'(form) {
    const list = await api('POST', '/readlists', { name: form.name.value });
    if (form.dataset.id) await api('PUT', `/readlists/${list.id}/entries/${form.dataset.id}`);
    location.reload();
  },
};

const run = async (el, fn) => {
  try { await busy(el, () => fn(el)); } catch (err) { toast(err.message); }
};

document.addEventListener('click', (e) => {
  const el = e.target.closest('button[data-action], a[data-action]');
  if (!el || !actions[el.dataset.action]) return;
  e.preventDefault();
  run(el, actions[el.dataset.action]);
});

document.addEventListener('submit', (e) => {
  const fn = submits[e.target.dataset.action];
  if (!fn) return;
  e.preventDefault();
  run(e.submitter || e.target, () => fn(e.target));
});

document.addEventListener('change', async (e) => {
  const el = e.target;
  try {
    if (el.name === 'preserve' && el.form) syncSince(el.form);
    if (el.dataset.action === 'toggle-list') {
      await api(el.checked ? 'PUT' : 'DELETE', `/readlists/${el.dataset.list}/entries/${el.dataset.id}`);
      // The toolbar's bookmark is filled while the article is in any readlist.
      const member = !!document.querySelector('#list-checks input:checked');
      const button = document.getElementById('lists-button');
      if (button) {
        button.classList.toggle('on', member);
        button.title = member ? 'In a readlist' : 'Add to a readlist';
      }
      // Opened from a row of a list: that row's mark and the sidebar counts follow.
      const row = document.querySelector(`#entries .entry[data-id="${el.dataset.id}"]`);
      if (row) {
        row.classList.toggle('listed', member);
        refreshCounts();
      }
    }
    if (el.dataset.action === 'import-opml' && el.files[0]) {
      const body = new FormData();
      body.append('file', el.files[0]);
      const { added, skipped } = await api('POST', '/import/opml', body);
      toast(`Imported ${added} feeds${skipped ? `, ${skipped} already present` : ''}`);
      setTimeout(() => location.reload(), 1200);
    }
  } catch (err) { toast(err.message); }
});

// Clearing the search box (the field's own "x", or deleting the text) brings the
// full list back straight away instead of waiting for Enter.
document.addEventListener('input', (e) => {
  const el = e.target;
  if (el.type !== 'search' || el.name !== 'q' || el.value !== '') return;
  if (new URLSearchParams(location.search).get('q')) el.form.requestSubmit();
});

// An empty search is no search: keep "q=" out of the address.
document.addEventListener('submit', (e) => {
  const q = e.target.elements && e.target.elements.q;
  if (q && q.type === 'search' && q.value.trim() === '') q.disabled = true;
});

// Display preferences apply immediately and are stored on this device.
document.addEventListener('click', (e) => {
  const button = e.target.closest('#prefs-dialog [data-pref] button');
  if (!button) return;
  const name = button.closest('[data-pref]').dataset.pref;
  if (name === 'size') readroomPrefs.set({ size: readroomPrefs.get().size + Number(button.value) });
  else readroomPrefs.set({ [name]: button.value });
  syncPrefs();
});

// A row menu is pinned to the viewport, so it closes rather than drift when the page scrolls.
addEventListener('scroll', () => {
  const menu = document.querySelector('.menu:popover-open');
  if (menu) menu.hidePopover();
}, { passive: true });

// Close a sheet when its backdrop is tapped.
document.addEventListener('click', (e) => {
  if (e.target instanceof HTMLDialogElement) e.target.close();
});


// ---- next / previous article ------------------------------------------------
// An article has no order of its own: "next" means next in the list it was opened
// from. The list hands over what it showed (kept for this tab only), and the
// article page works out its neighbours from that, loading further pages as needed.
const listKey = 'readroom-list';
const readList = () => {
  try { return JSON.parse(sessionStorage.getItem(listKey)); } catch { return null; }
};
const writeList = (list) => {
  try { sessionStorage.setItem(listKey, JSON.stringify(list)); } catch { /* no neighbours then */ }
};
const rowsOf = (root) => [...root.querySelectorAll('.entry[data-id]')].map((row) => (
  { id: +row.dataset.id, title: row.querySelector('.title').textContent }
));
const moreOf = (root) => {
  const more = root.querySelector('[data-action="load-more"]');
  return more ? more.dataset.url : '';
};
const rememberList = () => {
  const entries = document.getElementById('entries');
  if (entries) writeList({ back: location.pathname + location.search, items: rowsOf(entries), more: moreOf(entries) });
};
const openArticle = (href) => { rememberList(); location.href = href; };

document.addEventListener('click', (e) => {
  if (e.target.closest('#entries .entry > a')) rememberList();
});

const article = document.querySelector('.article[data-id]');
const neighbours = { prev: null, next: null };
const showNeighbours = async () => {
  const list = readList();
  const id = +article.dataset.id;
  let at = list ? list.items.findIndex((item) => item.id === id) : -1;
  if (at < 0) return;
  if (at === list.items.length - 1 && list.more) {
    // The last article of what the list had loaded: fetch its next page.
    const res = await fetch(list.more);
    if (res.ok) {
      const page = new DOMParser().parseFromString(await res.text(), 'text/html');
      list.items.push(...rowsOf(page));
      list.more = moreOf(page);
    }
  }
  list.current = id;
  writeList(list);
  document.querySelector('.topbar .back').href = list.back;
  const nav = document.querySelector('.neighbours');
  for (const [which, item] of [['prev', list.items[at - 1]], ['next', list.items[at + 1]]]) {
    if (!item) continue;
    const link = nav.querySelector('.' + which);
    link.href = `/entries/${item.id}`;
    link.querySelector('span').textContent = item.title || 'Untitled';
    link.hidden = false;
    nav.hidden = false;
    neighbours[which] = link;
  }
};
if (article) showNeighbours().catch(() => {});

// Moving between articles replaces the page, so Back still leads to the list.
document.addEventListener('click', (e) => {
  const link = e.target.closest('.neighbours a');
  if (!link) return;
  e.preventDefault();
  location.replace(link.href);
});

// ---- keyboard ---------------------------------------------------------------
// The selected row of a list; coming back from an article selects that article.
const cursor = () => document.querySelector('#entries .entry.cursor');
const setCursor = (row) => {
  const old = cursor();
  if (old) old.classList.remove('cursor');
  row.classList.add('cursor');
  row.scrollIntoView({ block: 'nearest' });
};
const moveCursor = async (step) => {
  const rows = [...document.querySelectorAll('#entries .entry')];
  if (!rows.length) return;
  const at = rows.indexOf(cursor());
  if (step > 0 && at === rows.length - 1) {
    const more = document.querySelector('#entries [data-action="load-more"]');
    if (!more) return;
    await actions['load-more'](more);
    return setCursor(document.querySelectorAll('#entries .entry')[at + 1] || rows[at]);
  }
  setCursor(rows[Math.max(0, Math.min(rows.length - 1, at < 0 ? 0 : at + step))]);
};
const restoreCursor = () => {
  const list = readList();
  if (!list || !list.current || list.back !== location.pathname + location.search) return;
  const row = document.querySelector(`#entries .entry[data-id="${list.current}"]`);
  // Once only: a later visit to this list (a tab, the sidebar) must not jump to that row again.
  delete list.current;
  writeList(list);
  if (row) setCursor(row);
};
if (document.getElementById('entries')) addEventListener('pageshow', restoreCursor);

const click = (selector) => {
  const el = document.querySelector(selector);
  if (el) el.click();
};
const listKeys = {
  j: () => moveCursor(1),
  k: () => moveCursor(-1),
  Enter: () => { const row = cursor(); if (row) openArticle(row.querySelector('a').href); },
  m: () => { const row = cursor(); if (row) return setRead(row, !row.classList.contains('read')); },
  s: () => { const row = cursor(); if (row) return setStar(row, !row.classList.contains('starred')); },
  h: () => {
    const row = cursor();
    if (!row) return;
    // Keep a row selected: the next one, or the previous at the end of the list.
    const next = row.nextElementSibling && row.nextElementSibling.matches('.entry') ? row.nextElementSibling : row.previousElementSibling;
    if (next) setCursor(next);
    return hideRow(row);
  },
  '/': () => document.querySelector('.search input').focus(),
};
listKeys.o = listKeys.Enter;
const articleKeys = {
  j: () => { if (neighbours.next) location.replace(neighbours.next.href); else toast('No next article in this list'); },
  k: () => { if (neighbours.prev) location.replace(neighbours.prev.href); else toast('No previous article in this list'); },
  u: () => click('.topbar .back'),
  Escape: () => click('.topbar .back'),
  m: () => click('[data-action="mark-unread"]'),
  s: () => click('.actions [data-action="toggle-star"]'),
  h: () => click('[data-action="hide-entry"]'),
  v: () => click('.actions a[target="_blank"]'),
};
document.addEventListener('keydown', (e) => {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
  if (!(e.target instanceof Element) || e.target.closest('input, textarea, select, [contenteditable]')) return;
  if (document.querySelector('dialog[open], [popover]:popover-open')) return;
  const keys = article ? articleKeys : document.getElementById('entries') ? listKeys : {};
  const run = e.key === '?' ? () => click('[data-dialog="keys-dialog"]') : keys[e.key];
  if (!run) return;
  e.preventDefault();
  Promise.resolve().then(run).catch((err) => toast(err.message));
});
