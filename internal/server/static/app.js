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

let toastTimer;
const toast = (msg) => {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 2600);
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

const actions = {
  async 'toggle-star'(el) {
    const on = !el.classList.contains('on');
    await api('PATCH', `/entries/${el.dataset.id}`, { starred: on });
    el.classList.toggle('on', on);
    el.setAttribute('aria-pressed', on);
  },
  async 'mark-unread'(el) {
    await api('PATCH', `/entries/${el.dataset.id}`, { read: false });
    history.length > 1 ? history.back() : (location.href = '/');
  },
  async preserve(el) {
    await api('POST', `/entries/${el.dataset.id}/preserve`);
    toast('Saving a local copy…');
    // The copy is made in the background; pick it up once it is ready.
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 1500));
      const e = await api('GET', `/entries/${el.dataset.id}`);
      if (e.preserved) return location.reload();
      if (e.preserve_error) return toast('Could not save: ' + e.preserve_error);
    }
  },
  async 'mark-all-read'(el) {
    const body = {};
    if (+el.dataset.feed) body.feed_id = +el.dataset.feed;
    if ('category' in el.dataset) body.category = el.dataset.category;
    const { marked } = await api('POST', '/entries/mark-read', body);
    if (!marked) return toast('Nothing unread here');
    location.reload();
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
  'reset-prefs'() { readroomPrefs.reset(); syncPrefs(); },
  'close-dialog'(el) { el.closest('dialog').close(); },

  'edit-feed'(el) {
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
    syncSince(form);
    feedDialog().showModal();
  },
  async 'refresh-feed'(el) {
    const feed = await api('POST', `/feeds/${el.closest('form').dataset.id}/refresh`);
    toast(`Refreshed · ${feed.total} articles`);
  },
  async 'delete-feed'(el) {
    const form = el.closest('form');
    if (!confirm(`Delete "${form.title.value || form.title.placeholder}" and all its articles?`)) return;
    await api('DELETE', `/feeds/${form.dataset.id}`);
    location.reload();
  },
  // The row menu is one shared popover, placed under the kebab that opened it.
  'row-menu'(el) {
    const menu = document.getElementById('row-menu');
    menu.querySelectorAll('button').forEach((b) => { b.dataset.id = el.dataset.id; });
    menu.showPopover();
    const at = el.getBoundingClientRect();
    const below = at.bottom + 4 + menu.offsetHeight <= innerHeight - 8;
    menu.style.top = (below ? at.bottom + 4 : at.top - 4 - menu.offsetHeight) + 'px';
    menu.style.left = Math.max(8, at.right - menu.offsetWidth) + 'px';
  },
  async 'remove-from-list'(el) {
    el.closest('[popover]').hidePopover();
    await api('DELETE', `/readlists/${el.dataset.list}/entries/${el.dataset.id}`);
    document.querySelector(`#entries .entry[data-id="${el.dataset.id}"]`).remove();
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
    const feed = await api('POST', '/feeds', { url: form.url.value, category: form.category.value });
    if (feed.last_error) toast('Added, but the fetch failed: ' + feed.last_error);
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
