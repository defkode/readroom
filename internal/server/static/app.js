// The web app is a thin client of the same REST API that scripts use.
//
// Everything here drives one page. It runs once per page: when a page loads,
// and again when player.js swaps another page's content in. "signal" ends with
// the page, and takes its listeners with it; "soft" says the page was swapped in.
window.readroomBoot = (signal, soft) => {
  const on = (target, type, fn, opts) => target.addEventListener(type, fn,
    Object.assign({ signal }, typeof opts === 'boolean' ? { capture: opts } : opts));

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
    readroomFolds.apply(); // the fresh copy arrives with every category unfolded
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
    readroomNav.reload();
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
      readroomNav.reload();
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
          readroomNav.reload();
        } });
      } else {
        toast(`Marked ${undo.ids.length} as read`, { label: 'Undo', run: async () => {
          await api('POST', '/entries/mark-unread', { ids: undo.ids });
          readroomNav.reload();
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
  const showOutage = () => {
    const outage = document.getElementById('outage');
    if (!outage) return;
    let dismissed = null;
    try { dismissed = localStorage.getItem(outageKey); } catch { /* always shown */ }
    outage.hidden = dismissed === outage.dataset.key;
  };
  showOutage();

  // Hiding puts an article out of every list for good; the row goes at once and
  // the message that follows can bring it back.
  const hideRow = async (row) => {
    const id = row.dataset.id;
    await api('PATCH', `/entries/${id}`, { hidden: true });
    row.remove();
    if (!document.querySelector('#entries .entry')) return readroomNav.reload();
    refreshCounts();
    toast('Hidden', { label: 'Undo', run: async () => {
      await api('PATCH', `/entries/${id}`, { hidden: false });
      readroomNav.reload();
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
      history.length > 1 ? history.back() : readroomNav.go('/');
    },
    async preserve(el) {
      if (await keepCopy(el.dataset.id)) readroomNav.reload();
    },
    async unpreserve(el) {
      if (await dropCopy(el.dataset.id)) readroomNav.reload();
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
      if (await keepCopy(el.dataset.id)) readroomNav.reload();
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
    // Ask for the article to be read aloud; once asked, the button leads to where that stands.
    async 'request-audio'(el) {
      const box = document.getElementById('audio');
      if (box.dataset.state && !('again' in el.dataset)) return box.scrollIntoView({ block: 'center', behavior: 'smooth' });
      await api('POST', `/entries/${el.dataset.id}/audio`);
      await refreshAudio();
      document.getElementById('audio').scrollIntoView({ block: 'center', behavior: 'smooth' });
    },
    // The recording plays in the app's player, so it goes on while other pages are read.
    'play-audio'(el) {
      readroomPlayer.play({ id: el.dataset.id, src: el.dataset.src, title: el.dataset.title, feed: el.dataset.feed });
    },
    'row-listen'(el) {
      const row = rowOf(el);
      closeMenu(el);
      readroomPlayer.play({
        id: el.dataset.id, src: `/api/v1/entries/${el.dataset.id}/audio`,
        title: row.querySelector('.title').textContent, feed: row.querySelector('.feed').textContent,
      });
    },
    async 'delete-audio'(el) {
      if ('confirm' in el.dataset && !confirm('Remove the recording of this article?')) return;
      await api('DELETE', `/entries/${el.dataset.id}/audio`);
      const playing = readroomPlayer.state();
      if (playing && playing.id === +el.dataset.id) readroomPlayer.close();
      await refreshAudio();
    },
    // Ask for the article in the owner's language; where that stands is shown under the title.
    async 'request-translation'(el) {
      el.disabled = true;
      await api('POST', `/entries/${el.dataset.id}/translation`);
      await refreshTranslation(location.href);
    },
    async 'delete-translation'(el) {
      if ('confirm' in el.dataset && !confirm('Remove the translation of this article?')) return;
      await api('DELETE', `/entries/${el.dataset.id}/translation`);
      // The text on the page may be the translation that is now gone.
      readroomNav.replace(location.pathname);
    },
    // The other version of the article takes this one's place, in the page and in the history.
    'show-version'(el) { readroomNav.replace(el.dataset.href); },
    // Go on from where the article was left, here or on another device.
    'resume-reading'() { resume(true); },
    'dismiss-resume'() { resume(false); },
    // Take an article off "Continue reading": it counts as not begun.
    async 'forget-position'(el) {
      await api('PATCH', `/entries/${el.dataset.id}`, { position: 0 });
      const list = el.closest('ul');
      el.closest('li').remove();
      if (!list.querySelector('li')) { list.previousElementSibling.remove(); list.remove(); }
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
      if (!document.querySelector('#entries .entry')) return readroomNav.reload();
      toast('Shown again');
    },
    // From the article: hide it and go back to the list, which offers the undo.
    async 'hide-entry'(el) {
      await api('PATCH', `/entries/${el.dataset.id}`, { hidden: true });
      try { sessionStorage.setItem(undoKey, JSON.stringify({ hidden: +el.dataset.id, at: Date.now() })); } catch { /* no undo then */ }
      readroomNav.go(document.querySelector('.topbar .back').href);
    },
    async 'unhide-entry'(el) {
      await api('PATCH', `/entries/${el.dataset.id}`, { hidden: false });
      readroomNav.reload();
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
      form.cleanup.checked = d.cleanup === 'true';
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
      readroomNav.reload();
    },
    'dismiss-outage'(el) {
      const notice = el.closest('.notice');
      try { localStorage.setItem(outageKey, notice.dataset.key); } catch { /* shown again next time */ }
      notice.hidden = true;
    },
    async unsubscribe(el) {
      const form = el.closest('form');
      if (!confirm(`Unsubscribe from "${form.title.value || form.title.placeholder}"?\n\nIts articles are deleted with it.`)) return;
      await api('DELETE', `/feeds/${form.dataset.id}`);
      // Unsubscribed from the feed's own list, there is nothing left here to show.
      new URLSearchParams(location.search).has('feed_id') ? readroomNav.go('/') : readroomNav.reload();
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
        menu.querySelector('[data-action="row-listen"]').hidden = !row.classList.contains('spoken');
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
      if (!document.querySelector('#entries .entry')) return readroomNav.reload();
      const count = document.querySelector(`a[href="/?readlist_id=${el.dataset.list}"] b`);
      if (count) count.textContent = Math.max(0, count.textContent - 1);
      toast('Removed from readlist');
    },
    'edit-list'(el) {
      closeMenu(el);
      const dialog = document.getElementById('list-dialog');
      const form = dialog.querySelector('form');
      form.dataset.id = el.dataset.id;
      form.dataset.name = form.name.value = el.dataset.name;
      dialog.showModal();
    },
    // From Manage readlists the button names the list; in a list's own settings its form does.
    async 'delete-list'(el) {
      const { id, name } = (el.closest('#list-dialog form') || el).dataset;
      if (!confirm(`Delete readlist "${name}"? Its articles are kept.`)) return;
      await api('DELETE', `/readlists/${id}`);
      // Deleted from the readlist's own list, there is nothing left here to show.
      new URLSearchParams(location.search).has('readlist_id') ? readroomNav.go('/readlists') : readroomNav.reload();
    },
  };

  const submits = {
    async subscribe(form) {
      const feed = await api('POST', '/feeds', { url: form.url.value, category: form.category.value, unread: form.unread.value });
      // A feed that sends only the opening of each article: offer to fetch the rest.
      if (feed.teaser_only && !feed.preserve &&
          confirm(`"${feed.title}" sends only a teaser of each article.\n\nKeep local copies, so the full text and images are here?`)) {
        await api('PATCH', `/feeds/${feed.id}`, { preserve: true });
      }
      // The reload shows the feed in its place; say so once the page is back.
      try { sessionStorage.setItem(addedKey, JSON.stringify({ id: feed.id, title: feed.title, total: feed.total, at: Date.now() })); } catch { /* no confirmation then */ }
      readroomNav.reload();
    },
    // Save the article at an address. The reload shows it at the top of the list.
    async 'save-article'(form) {
      toast('Fetching the article…');
      const { created, entry } = await api('POST', '/articles', { url: form.url.value });
      try { sessionStorage.setItem(savedKey, JSON.stringify({ id: entry.id, title: entry.title, created, at: Date.now() })); } catch { /* no confirmation then */ }
      // Without the address that was handed over, so coming back here does not save it again.
      readroomNav.replace(`/?feed_id=${entry.feed_id}`);
    },
    async 'save-feed'(form) {
      await api('PATCH', `/feeds/${form.dataset.id}`, {
        title: form.title.value,
        url: form.url.value,
        category: form.category.value,
        preserve: form.preserve.checked,
        preserve_since: form.since.value || null,
        ai_summary: form.ai.checked,
        ai_cleanup: form.cleanup.checked,
        keep_days: +form.keep.value,
      });
      readroomNav.reload();
    },
    async 'save-list'(form) {
      await api('PATCH', `/readlists/${form.dataset.id}`, { name: form.name.value });
      readroomNav.reload();
    },
    // The field's pattern has already held the form back unless it says "reset".
    async 'reset-database'(form) {
      await api('POST', '/reset', { confirm: form.confirm.value });
      try { sessionStorage.clear(); } catch { /* nothing kept then */ } // lists and undo offers of articles that are gone
      readroomNav.go('/feeds');
    },
    async 'create-list'(form) {
      const list = await api('POST', '/readlists', { name: form.name.value });
      if (form.dataset.id) await api('PUT', `/readlists/${list.id}/entries/${form.dataset.id}`);
      readroomNav.reload();
    },
  };

  // After subscribing the page reloads: confirm it and point at the new row.
  const addedKey = 'readroom-added-feed';
  try {
    const added = JSON.parse(sessionStorage.getItem(addedKey));
    sessionStorage.removeItem(addedKey);
    if (added && Date.now() - added.at < 5000) {
      const articles = added.total === 1 ? '1 article' : `${added.total || 0} articles`;
      toast(`Subscribed to “${added.title}” · ${articles}`);
      // A failing feed is listed twice; the last match is its place among the categories.
      const row = [...document.querySelectorAll(`.rowbtn[data-action="edit-feed"][data-id="${added.id}"]`)].pop();
      if (row) {
        row.closest('li').classList.add('fresh');
        row.scrollIntoView({ block: 'center' });
      }
    }
  } catch { /* storage unavailable */ }

  // After saving an article the list is shown afresh: confirm it and point at the row.
  const savedKey = 'readroom-saved-article';
  try {
    const saved = JSON.parse(sessionStorage.getItem(savedKey));
    sessionStorage.removeItem(savedKey);
    if (saved && Date.now() - saved.at < 5000) {
      toast(saved.created ? `Saved “${saved.title}”` : 'Already saved', saved.created ? undefined
        : { label: 'Open', run: async () => readroomNav.go(`/entries/${saved.id}`) });
      const row = document.querySelector(`#entries .entry[data-id="${saved.id}"]`);
      if (row) row.classList.add('fresh');
    }
  } catch { /* storage unavailable */ }

  const run = async (el, fn) => {
    try { await busy(el, () => fn(el)); } catch (err) { toast(err.message); }
  };

  on(document, 'click', (e) => {
    // A download from a menu leaves the page where it is, so the menu has to go by itself.
    const download = e.target.closest('[popover] a[download]');
    if (download) closeMenu(download);
    const el = e.target.closest('button[data-action], a[data-action]');
    if (!el || !actions[el.dataset.action]) return;
    e.preventDefault();
    run(el, actions[el.dataset.action]);
  });

  on(document, 'submit', (e) => {
    const fn = submits[e.target.dataset.action];
    if (!fn) return;
    e.preventDefault();
    run(e.submitter || e.target, () => fn(e.target));
  });

  // An address shared from another app arrives in the form already; save it.
  const handedOver = document.querySelector('form[data-auto]');
  if (handedOver) handedOver.requestSubmit(handedOver.querySelector('button'));

  on(document, 'change', async (e) => {
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
        setTimeout(() => readroomNav.reload(), 1200);
      }
    } catch (err) { toast(err.message); }
  });

  // ---- search and period -------------------------------------------------------
  // A list is filtered where it stands: the page for the new search is fetched and
  // its list, tabs and menus take the place of the ones shown. The form itself is
  // never replaced, so the field keeps its focus, its caret and a phone its
  // keyboard. Typing filters after a second's pause; Enter, emptying the field
  // and choosing a period filter at once.
  const filterForm = document.querySelector('form.search');
  if (filterForm) {
    const picked = () => filterForm.q.value.trim() + '\n' + filterForm.period.value;
    let shown = picked(), timer, turn = 0;
    const filter = async () => {
      clearTimeout(timer);
      const want = picked();
      if (want === shown) return filterForm.classList.remove('busy');
      const params = new URLSearchParams(new FormData(filterForm));
      // An empty search is no search: keep "q=" and "period=" out of the address.
      for (const name of ['q', 'period']) if (!params.get(name).trim()) params.delete(name);
      const url = '/?' + params;
      const mine = ++turn;
      filterForm.classList.add('busy');
      try {
        const res = await fetch(url, { signal });
        if (res.status === 401) { location.href = '/login'; return; }
        if (!res.ok) throw new Error(`Search failed (${res.status})`);
        const fresh = new DOMParser().parseFromString(await res.text(), 'text/html');
        if (mine !== turn || signal.aborted) return; // overtaken by a later search, or the page is gone
        for (const part of ['#lead', '#views', '#results', '#menus', '.topbar .actions']) {
          const from = fresh.querySelector(part), to = document.querySelector(part);
          if (from && to) to.replaceWith(document.adoptNode(from));
        }
        showOutage();
        filterForm.period.classList.toggle('on', filterForm.period.value !== '');
        history.replaceState(history.state, '', url);
        shown = want;
      } catch (err) {
        if (!signal.aborted) toast(err.message);
      } finally {
        if (mine === turn) filterForm.classList.remove('busy');
      }
    };
    on(filterForm.q, 'input', () => {
      clearTimeout(timer);
      if (filterForm.q.value.trim() === '') return filter();
      filterForm.classList.toggle('busy', picked() !== shown);
      timer = setTimeout(filter, 1000);
    });
    on(filterForm.period, 'change', filter);
    on(filterForm, 'submit', (e) => {
      e.preventDefault();
      filterForm.q.blur(); // Enter ends the typing: a phone puts its keyboard away
      filter();
    });
    // Not on(): a listener tied to the signal is taken away before the signal reports its end.
    signal.addEventListener('abort', () => clearTimeout(timer));
  }

  // Display preferences apply immediately and are stored on this device.
  on(document, 'click', (e) => {
    const button = e.target.closest('#prefs-dialog [data-pref] button');
    if (!button) return;
    const name = button.closest('[data-pref]').dataset.pref;
    if (name === 'size') readroomPrefs.set({ size: readroomPrefs.get().size + Number(button.value) });
    else readroomPrefs.set({ [name]: button.value });
    syncPrefs();
  });

  // A row menu is pinned to the viewport, so it closes rather than drift when the page scrolls.
  on(window, 'scroll', () => {
    const menu = document.querySelector('.menu:popover-open');
    if (menu) menu.hidePopover();
  }, { passive: true });

  // Close a sheet when its backdrop is tapped.
  on(document, 'click', (e) => {
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
  const openArticle = (href) => { rememberList(); readroomNav.go(href); };

  on(document, 'click', (e) => {
    if (e.target.closest('#entries .entry > a')) rememberList();
  });

  // An AI summary in a list is cut to three lines. A click on it shows the whole
  // of it in place, and again folds it, without opening the article or marking it read;
  // the rest of the row still opens the article.
  const toggleSummary = (row) => {
    const snippet = row.querySelector('.snippet.whole');
    if (!snippet) return false;
    const open = row.classList.toggle('open');
    snippet.title = open ? 'Fold the summary' : 'Show the whole summary';
    return true;
  };
  on(document, 'click', (e) => {
    if (e.button || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return; // opening in a new tab still works
    const snippet = e.target.closest('#entries .entry .snippet.whole');
    if (!snippet || getSelection().toString()) return;
    e.preventDefault();
    toggleSummary(snippet.closest('.entry'));
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

  // ---- listening ---------------------------------------------------------------
  // A recording is made outside the server, some time after it is asked for. The
  // part of the page that shows where it stands is taken from a fresh copy of the
  // page whenever that changes, and while one is awaited the article is asked now
  // and then whether it has arrived.
  const refreshAudio = async () => {
    const res = await fetch(location.href);
    if (!res.ok || signal.aborted) return;
    const fresh = new DOMParser().parseFromString(await res.text(), 'text/html');
    for (const part of ['#audio', '#audio-button']) {
      const from = fresh.querySelector(part), to = document.querySelector(part);
      if (from && to) to.replaceWith(from);
    }
    watchAudio();
    showPlaying();
  };
  let audioTimer;
  const watchAudio = () => {
    clearTimeout(audioTimer);
    const box = document.getElementById('audio');
    if (!box || box.dataset.state !== 'queued' || document.hidden) return;
    audioTimer = setTimeout(async () => {
      try {
        const entry = await api('GET', `/entries/${article.dataset.id}`);
        if (entry.audio !== 'queued') {
          await refreshAudio();
          if (entry.audio === 'ready') toast('The recording is ready');
          return;
        }
      } catch { /* try again next time */ }
      watchAudio();
    }, 5000);
  };
  signal.addEventListener('abort', () => clearTimeout(audioTimer));
  // The article's Listen button follows what the player is doing with its recording.
  const showPlaying = () => {
    const button = document.querySelector('#audio [data-action="play-audio"]');
    if (!button) return;
    const state = readroomPlayer.state();
    const mine = state && state.id === +button.dataset.id;
    button.textContent = mine && state.playing ? 'Pause' : mine ? 'Resume' : 'Listen';
  };

  // ---- translation --------------------------------------------------------------
  // Made outside the server like a recording, and watched for the same way. When it
  // arrives the text being read is left alone: the notice offers the translation.
  const refreshTranslation = async (from) => {
    const res = await fetch(from);
    if (!res.ok || signal.aborted) return;
    const fresh = new DOMParser().parseFromString(await res.text(), 'text/html');
    for (const part of ['#translation', '.article .status']) {
      const now = fresh.querySelector(part), old = document.querySelector(part);
      if (now && old) old.replaceWith(now);
    }
    watchTranslation();
  };
  let translationTimer;
  const watchTranslation = () => {
    clearTimeout(translationTimer);
    const box = document.getElementById('translation');
    if (!box || box.dataset.state !== 'queued' || document.hidden) return;
    translationTimer = setTimeout(async () => {
      try {
        const entry = await api('GET', `/entries/${article.dataset.id}`);
        if (entry.translation !== 'queued') {
          await refreshTranslation(`${location.pathname}?original=1`);
          if (entry.translation === 'ready') toast('The translation is ready');
          return;
        }
      } catch { /* try again next time */ }
      watchTranslation();
    }, 5000);
  };
  signal.addEventListener('abort', () => clearTimeout(translationTimer));
  if (article) {
    watchTranslation();
    on(document, 'visibilitychange', watchTranslation);
  }
  if (article) {
    watchAudio();
    showPlaying();
    on(document, 'visibilitychange', watchAudio);
    on(document, 'readroom:player', showPlaying);
  }

  // ---- reading position ---------------------------------------------------------
  // How far the article has been read is kept on the server, so another device can
  // offer to go on from there. It is a share of the article's text, not of the
  // page's height: the same sentence on a phone and on a wide screen.
  const content = article && article.querySelector('.content');
  let resume = () => {};
  if (content) {
    const texts = () => {
      const walker = document.createTreeWalker(content, NodeFilter.SHOW_TEXT);
      const nodes = [];
      let total = 0;
      for (let node; (node = walker.nextNode());) {
        const length = node.data.trim().length;
        if (!length) continue;
        nodes.push({ node, at: total });
        total += length;
      }
      return { nodes, total };
    };
    const boxOf = (node) => {
      const range = document.createRange();
      range.selectNodeContents(node);
      return range.getBoundingClientRect();
    };
    // Where reading happens: just under the bar that stays at the top.
    const edge = () => document.querySelector('.topbar').getBoundingClientRect().bottom + 8;
    const measure = () => {
      const { nodes, total } = texts();
      if (!total) return null;
      if (content.getBoundingClientRect().bottom <= innerHeight) return 1; // its end is on the screen
      const line = edge();
      let low = 0, high = nodes.length - 1;
      while (low < high) {
        const mid = (low + high) >> 1;
        if (boxOf(nodes[mid].node).bottom > line) high = mid; else low = mid + 1;
      }
      return nodes[low].at / total;
    };
    const seek = (position) => {
      const { nodes, total } = texts();
      const target = position * total;
      let at = 0;
      while (at < nodes.length - 1 && nodes[at + 1].at <= target) at++;
      if (nodes.length) scrollTo({ top: scrollY + boxOf(nodes[at].node).top - edge(), behavior: 'smooth' });
    };

    // An article that fits on a screen or so has no place to come back to.
    const long = () => content.offsetHeight > innerHeight * 1.5;
    const stored = +article.dataset.position || 0;
    const prompt = document.getElementById('resume');
    // While the offer stands, the place kept is only ever moved forward: looking at
    // the top of the article must not lose where it was left.
    let asking = false, sent = stored, timer;
    const save = (keepalive) => {
      clearTimeout(timer);
      if (!long()) return;
      const position = measure();
      if (position === null || Math.abs(position - sent) < 0.005 || (asking && position < sent)) return;
      sent = position;
      fetch(`/api/v1/entries/${article.dataset.id}`, {
        method: 'PATCH', keepalive, headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ position: Math.round(position * 1000) / 1000 }),
      }).catch(() => { /* the next report will do */ });
    };
    resume = (go) => {
      asking = false;
      if (prompt) prompt.hidden = true;
      if (go) seek(stored);
    };
    if (prompt && stored && long()) {
      // Not when the browser has already put the page where it was (coming back to it).
      const here = measure();
      asking = here === null || Math.abs(here - stored) > 0.02;
      prompt.hidden = !asking;
    }
    on(window, 'scroll', () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        // At the place that was kept, or past it: the offer has nothing left to say.
        if (asking && measure() >= stored - 0.02) resume(false);
        save(false);
      }, 1200);
    }, { passive: true });
    on(document, 'visibilitychange', () => { if (document.hidden) save(true); });
    on(window, 'pagehide', () => save(true));
    signal.addEventListener('abort', () => save(true)); // another page is swapped in
  }

  // A browser names a saved print after the page's title: for the time of printing
  // that is a short name instead of the whole headline and the app's name.
  if (article && article.dataset.printName) {
    const title = document.title;
    on(window, 'beforeprint', () => { document.title = article.dataset.printName; });
    on(window, 'afterprint', () => { document.title = title; });
  }

  // Moving between articles replaces the page, so Back still leads to the list.
  on(document, 'click', (e) => {
    const link = e.target.closest('.neighbours a');
    if (!link) return;
    e.preventDefault();
    readroomNav.replace(link.href);
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
  if (document.getElementById('entries')) {
    on(window, 'pageshow', restoreCursor);
    if (soft) restoreCursor(); // swapped in: there is no page load to wait for
  }

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
    e: () => { const row = cursor(); if (row && !toggleSummary(row)) toast('No AI summary for this article'); },
    h: () => {
      const row = cursor();
      if (!row) return;
      // Keep a row selected: the next one, or the previous at the end of the list.
      const next = row.nextElementSibling && row.nextElementSibling.matches('.entry') ? row.nextElementSibling : row.previousElementSibling;
      if (next) setCursor(next);
      return hideRow(row);
    },
    '/': () => document.querySelector('.search input[type=search]').focus(),
  };
  listKeys.o = listKeys.Enter;
  const articleKeys = {
    j: () => { if (neighbours.next) readroomNav.replace(neighbours.next.href); else toast('No next article in this list'); },
    k: () => { if (neighbours.prev) readroomNav.replace(neighbours.prev.href); else toast('No previous article in this list'); },
    u: () => click('.topbar .back'),
    Escape: () => click('.topbar .back'),
    m: () => click('[data-action="mark-unread"]'),
    s: () => click('.actions [data-action="toggle-star"]'),
    h: () => click('[data-action="hide-entry"]'),
    v: () => click('.actions a[target="_blank"]'),
  };
  on(document, 'keydown', (e) => {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    if (!(e.target instanceof Element) || e.target.closest('input, textarea, select, [contenteditable]')) return;
    if (document.querySelector('dialog[open], [popover]:popover-open')) return;
    const keys = article ? articleKeys : document.getElementById('entries') ? listKeys : {};
    const run = e.key === '?' ? () => click('[data-dialog="keys-dialog"]') : keys[e.key];
    if (!run) return;
    e.preventDefault();
    Promise.resolve().then(run).catch((err) => toast(err.message));
  });
};
readroomNav.start(window.readroomBoot);
