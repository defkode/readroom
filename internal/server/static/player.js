// The player and what keeps it playing.
//
// A recording is played by one audio element that belongs to no page, shown as
// a bar at the bottom of the app. Every link here loads a new page, which would
// end the sound; so while something is loaded in the player, moving about the
// app swaps the page's content in place instead (the same HTML the server would
// have sent, fetched and put where the old one was), and the script that
// drives a page is started again for the new one. With nothing in the player,
// pages load the ordinary way.
(function () {
  const STORE = 'readroom-player'; // what is loaded and how far in: survives a real page load in this tab
  const RATE = 'readroom:rate';    // playback speed, per device
  const RATES = [1, 1.25, 1.5, 1.75, 2];
  const root = document.documentElement;
  const audio = new Audio();
  audio.preload = 'metadata';

  let now = null; // what is loaded: {id, title, feed, src}
  let bar, boot, ctl, saved = 0;

  const clock = (s) => {
    if (!isFinite(s)) return '–:––';
    s = Math.floor(s);
    const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60);
    return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(s % 60).padStart(2, '0');
  };
  const icon = (name) => `<svg class="icon" aria-hidden="true"><use href="#i-${name}"/></svg>`;
  const rate = () => {
    try { return RATES.includes(+localStorage.getItem(RATE)) ? +localStorage.getItem(RATE) : 1; } catch { return 1; }
  };
  const remember = () => {
    try {
      if (now) sessionStorage.setItem(STORE, JSON.stringify(Object.assign({ time: audio.currentTime }, now)));
      else sessionStorage.removeItem(STORE);
    } catch { /* starts over after a page load then */ }
  };
  // Pages show what the player is doing (the article's own Listen button).
  const announce = () => document.dispatchEvent(new CustomEvent('readroom:player'));

  const build = () => {
    bar = document.createElement('div');
    bar.id = 'player';
    bar.hidden = true;
    bar.setAttribute('role', 'region');
    bar.setAttribute('aria-label', 'Player');
    bar.innerHTML = `<div class="in">
      <button class="iconbtn toggle" type="button"></button>
      <a class="what"><b></b><small></small></a>
      <span class="time"></span>
      <button class="rate" type="button" title="Playback speed"></button>
      <button class="iconbtn close" type="button" aria-label="Close the player" title="Close the player">${icon('close')}</button>
      <input class="seek" type="range" min="0" max="1000" step="1" value="0" aria-label="Position">
    </div>`;
    document.body.append(bar);
    bar.querySelector('.toggle').onclick = toggle;
    bar.querySelector('.close').onclick = close;
    bar.querySelector('.rate').onclick = () => {
      const next = RATES[(RATES.indexOf(audio.playbackRate) + 1) % RATES.length];
      try { localStorage.setItem(RATE, next); } catch { /* until the page is left */ }
      audio.playbackRate = audio.defaultPlaybackRate = next;
    };
    const seek = bar.querySelector('.seek');
    seek.oninput = () => {
      if (isFinite(audio.duration)) audio.currentTime = seek.value / 1000 * audio.duration;
    };
  };

  const paint = () => {
    if (!now) return;
    const button = bar.querySelector('.toggle');
    const state = audio.paused ? 'play' : 'pause';
    if (button.dataset.state !== state) {
      button.dataset.state = state;
      button.innerHTML = icon(state);
      button.setAttribute('aria-label', audio.paused ? 'Play' : 'Pause');
    }
    bar.querySelector('.time').textContent = clock(audio.currentTime) + ' / ' + clock(audio.duration);
    bar.querySelector('.rate').textContent = audio.playbackRate + '×';
    const seek = bar.querySelector('.seek');
    if (document.activeElement !== seek || audio.paused) {
      seek.value = isFinite(audio.duration) && audio.duration ? audio.currentTime / audio.duration * 1000 : 0;
    }
  };

  // The lock screen, headphone buttons and the like.
  const session = () => {
    if (!('mediaSession' in navigator)) return;
    const media = navigator.mediaSession;
    if (!now) { media.metadata = null; return; }
    media.metadata = new MediaMetadata({
      title: now.title, artist: now.feed, album: 'Readroom',
      artwork: [{ src: '/icon/512.png', sizes: '512x512', type: 'image/png' }],
    });
    const jump = (by) => { audio.currentTime = Math.max(0, Math.min(audio.duration || 0, audio.currentTime + by)); };
    const handlers = {
      play: () => audio.play(),
      pause: () => audio.pause(),
      seekbackward: (d) => jump(-(d.seekOffset || 15)),
      seekforward: (d) => jump(d.seekOffset || 30),
      seekto: (d) => { audio.currentTime = d.seekTime; },
    };
    for (const [action, run] of Object.entries(handlers)) {
      try { media.setActionHandler(action, run); } catch { /* not every browser has every action */ }
    }
  };
  const position = () => {
    if (!('mediaSession' in navigator) || !navigator.mediaSession.setPositionState || !isFinite(audio.duration)) return;
    try {
      navigator.mediaSession.setPositionState({ duration: audio.duration, playbackRate: audio.playbackRate, position: Math.min(audio.currentTime, audio.duration) });
    } catch { /* a position it does not like: it keeps its own count */ }
  };

  const load = (item, time) => {
    now = { id: +item.id, title: item.title || 'Untitled', feed: item.feed || '', src: item.src };
    audio.src = now.src;
    audio.playbackRate = audio.defaultPlaybackRate = rate();
    if (time) audio.addEventListener('loadedmetadata', () => { audio.currentTime = time; }, { once: true });
    const what = bar.querySelector('.what');
    what.href = '/entries/' + now.id;
    what.querySelector('b').textContent = now.title;
    what.querySelector('small').textContent = now.feed;
    bar.hidden = false;
    root.setAttribute('data-player', '');
    session();
    paint();
    remember();
    announce();
  };
  const toggle = () => {
    if (audio.paused) audio.play().catch(() => {}); else audio.pause();
  };
  const close = () => {
    audio.pause();
    audio.removeAttribute('src');
    audio.load();
    now = null;
    bar.hidden = true;
    root.removeAttribute('data-player');
    session();
    remember();
    announce();
  };

  for (const type of ['play', 'pause', 'durationchange', 'ratechange', 'seeked', 'ended']) {
    audio.addEventListener(type, () => { paint(); position(); remember(); announce(); });
  }
  audio.addEventListener('timeupdate', () => {
    paint();
    if (Math.abs(audio.currentTime - saved) >= 5) { saved = audio.currentTime; remember(); }
  });
  audio.addEventListener('error', () => {
    if (!now) return;
    close();
    const toast = document.getElementById('toast');
    if (toast) {
      toast.textContent = 'The recording could not be played';
      toast.classList.add('show');
      setTimeout(() => toast.classList.remove('show'), 2600);
    }
  });
  addEventListener('pagehide', remember);

  window.readroomPlayer = {
    // Plays a recording, or pauses and resumes it when it is the one already loaded.
    play(item) {
      if (now && now.id === +item.id) return toggle();
      load(item);
      audio.play().catch(() => {});
    },
    close,
    // What is loaded, for a page to show: null, or {id, playing}.
    state: () => (now ? { id: now.id, playing: !audio.paused } : null),
  };

  // ---- moving about without a page load --------------------------------------
  const version = (doc) => {
    const script = doc.querySelector('script[src^="/static/app.js"]');
    return script ? script.getAttribute('src') : '';
  };
  const hard = (url, opts) => {
    if (opts.replace) location.replace(url);
    else if (opts.push) location.href = url;
    else location.reload();
  };

  let visits = 0;
  const visit = async (url, opts) => {
    const turn = ++visits;
    let res, doc;
    try {
      res = await fetch(url, { headers: { Accept: 'text/html' } });
      // Anything that is not one of this release's pages is left to the browser:
      // a download, the sign-in page, a page of a newer release after an update.
      if (!(res.headers.get('Content-Type') || '').startsWith('text/html') || new URL(res.url).pathname === '/login') return hard(url, opts);
      doc = new DOMParser().parseFromString(await res.text(), 'text/html');
      if (version(doc) !== version(document)) return hard(url, opts);
    } catch {
      return hard(url, opts);
    }
    if (turn !== visits) return; // a later click overtook this one
    const from = location.pathname, to = new URL(res.url).pathname;

    const swap = () => {
      ctl.abort(); // the old page's listeners and timers go with it
      if (opts.push) {
        history.replaceState(Object.assign({}, history.state, { y: scrollY }), '');
        history.pushState({ soft: true }, '', res.url);
      } else if (opts.replace) {
        history.replaceState({ soft: true }, '', res.url);
      }
      document.title = doc.title;
      document.body.className = doc.body.className;
      for (const el of [...document.body.children]) if (el !== bar) el.remove();
      bar.before(...[...doc.body.children].map((el) => document.adoptNode(el)));
      readroomFolds.apply(); // scripts inside swapped content do not run
      scrollTo(0, opts.y || 0);
      ctl = new AbortController();
      boot(ctl.signal, true);
    };

    // The same animation a page load gets (see transitions.js and app.css).
    if (opts.plain || !document.startViewTransition || matchMedia('(prefers-reduced-motion: reduce)').matches) return swap();
    const direction = window.readroomDirection ? window.readroomDirection(from, to) : '';
    try {
      document.startViewTransition({ update: swap, types: direction ? [direction] : [] });
    } catch {
      document.startViewTransition(swap); // a browser that only takes the callback
    }
  };

  // Clicks and searches, after the page's own script had its say about them.
  addEventListener('click', (e) => {
    if (!now || e.defaultPrevented || e.button || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    const link = e.target.closest && e.target.closest('a[href]');
    if (!link || link.target || link.hasAttribute('download') || link.origin !== location.origin) return;
    if (/^\/(api|assets|static|icon)\//.test(link.pathname)) return;
    e.preventDefault();
    visit(link.href, { push: true });
  });
  addEventListener('submit', (e) => {
    const form = e.target;
    if (!now || e.defaultPrevented || form.method !== 'get' || new URL(form.action).origin !== location.origin) return;
    e.preventDefault();
    const url = new URL(form.action);
    url.search = new URLSearchParams(new FormData(form)).toString();
    visit(url.href, { push: true });
  });
  // Back and forward between places reached this way. Once the player is closed
  // the browser takes over again, with a real load of wherever that leads.
  addEventListener('popstate', (e) => {
    if (now) visit(location.href, { y: e.state && e.state.y });
    else location.reload();
  });

  // What the page script uses instead of location, so that its own moves
  // (after an action, with the keyboard) keep the sound going too.
  window.readroomNav = {
    go(url) { now ? visit(url, { push: true }) : (location.href = url); },
    replace(url) { now ? visit(url, { replace: true }) : location.replace(url); },
    reload() { now ? visit(location.href, { y: scrollY, plain: true }) : location.reload(); },
    // Called once by the page script with the function that drives a page.
    start(fn) {
      boot = fn;
      ctl = new AbortController();
      build();
      try {
        const kept = JSON.parse(sessionStorage.getItem(STORE));
        if (kept && kept.src) load(kept, kept.time); // ready to go on, one tap away
      } catch { /* nothing was playing */ }
      boot(ctl.signal, false);
    },
  };
})();
