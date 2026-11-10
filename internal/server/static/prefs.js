// Display preferences live in this browser only (localStorage), so a phone and a
// desktop can each have their own. Loaded synchronously in <head> so the page is
// painted with the right theme and type from the first frame.
(function () {
  const KEY = 'readroom:prefs';
  const DEFAULTS = { theme: 'auto', font: 'literata', size: 18, leading: 'normal', width: 'medium', align: 'left', sidebar: 'shown' };
  const FONTS = {
    literata: 'Literata, "Iowan Old Style", "Palatino Linotype", Georgia, serif',
    serif: '"Iowan Old Style", "Palatino Linotype", Palatino, "Book Antiqua", Georgia, "Noto Serif", serif',
    sans: 'system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif',
  };
  const LEADING = { tight: 1.45, normal: 1.65, loose: 1.9 };
  const WIDTH = { narrow: '560px', medium: '680px', wide: '820px' };
  const SIZE = { min: 14, max: 28 };
  const root = document.documentElement;

  const clean = (p) => {
    const out = Object.assign({}, DEFAULTS, p);
    if (!['auto', 'light', 'sepia', 'dark'].includes(out.theme)) out.theme = DEFAULTS.theme;
    if (!FONTS[out.font]) out.font = DEFAULTS.font;
    if (!LEADING[out.leading]) out.leading = DEFAULTS.leading;
    if (!WIDTH[out.width]) out.width = DEFAULTS.width;
    if (!['left', 'justify'].includes(out.align)) out.align = DEFAULTS.align;
    if (!['shown', 'hidden'].includes(out.sidebar)) out.sidebar = DEFAULTS.sidebar;
    out.size = Math.min(SIZE.max, Math.max(SIZE.min, Math.round(+out.size) || DEFAULTS.size));
    return out;
  };

  const load = () => {
    try { return clean(JSON.parse(localStorage.getItem(KEY) || '{}')); } catch (e) { return clean({}); }
  };

  const apply = (p) => {
    if (p.theme === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', p.theme);
    // Only wide screens have a sidebar to hide; the stylesheet ignores this elsewhere.
    root.toggleAttribute('data-sidebar-hidden', p.sidebar === 'hidden');
    root.style.setProperty('--read-font', FONTS[p.font]);
    root.style.setProperty('--read-size', p.size + 'px');
    root.style.setProperty('--read-leading', LEADING[p.leading]);
    root.style.setProperty('--read-width', WIDTH[p.width]);
    root.style.setProperty('--read-align', p.align === 'justify' ? 'justify' : 'start');
    // Keep the browser / status bar colour in step with the page background.
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = getComputedStyle(root).getPropertyValue('--bg').trim() || meta.content;
  };

  let current = load();
  apply(current);
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => apply(current));
  // A change made in another tab of the same browser applies here too.
  addEventListener('storage', (e) => { if (e.key === KEY) { current = load(); apply(current); } });

  window.readroomPrefs = {
    get: () => Object.assign({}, current),
    set(patch) {
      current = clean(Object.assign({}, current, patch));
      try { localStorage.setItem(KEY, JSON.stringify(current)); } catch (e) { /* private mode: applies until reload */ }
      apply(current);
      return this.get();
    },
    reset() {
      // Resets how text looks; whether the sidebar is shown is not a reading setting.
      current = clean({});
      return this.set({ sidebar: load().sidebar });
    },
  };
})();
