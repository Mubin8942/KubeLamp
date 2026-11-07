'use strict';
/* Terminal keyword highlighting (MobaXterm style). Colours IPs, URLs, MAC addresses, timestamps
   and words such as error / warning / success in terminal output by inserting ANSI colour codes
   into the stream before xterm.js draws it.

   Rules that keep it from breaking anything:
   - escape sequences pass through untouched (never split, never coloured inside);
   - text the program already coloured is left alone: only plain text is highlighted;
   - full-screen programs (vim, less, top, k9s…) on the alternate screen are not touched;
   - colour codes take no space on screen, so cursor positions and line editing still line up. */

const TermHL = (() => {
  const B = '(?<![\\w.:-])';
  const oct = '(?:25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)';
  const h4 = '[0-9a-fA-F]{1,4}';
  const words = list => '\\b(?:' + list.join('|') + ')\\b';

  // order matters: earlier alternatives win at the same position
  const RULES = [
    ['url', '\\b(?:https?|wss?|ftp)://[^\\s\'"<>`]*[^\\s\'"<>`.,;:!?)\\]}]', '4;94'],
    ['ip4', B + `(?:${oct}\\.){3}${oct}(?:/(?:3[0-2]|[12]?\\d))?(?::\\d{1,5})?(?![\\w]|\\.\\d)`, '95'],
    ['ip6', `(?<![\\w:])(?:(?:${h4}:){7}${h4}|(?:${h4}:){1,6}:(?:${h4}(?::${h4}){0,5})?)(?:/\\d{1,3})?(?![\\w:])`, '95'],
    ['mac', '(?<![\\w:-])[0-9a-fA-F]{2}(?<msep>[:-])[0-9a-fA-F]{2}(?:\\k<msep>[0-9a-fA-F]{2}){4}(?![\\w:-])', '35'],
    ['klogE', '^[EF](?=\\d{4} \\d\\d:\\d\\d:\\d\\d)', '1;91'],
    ['klogW', '^W(?=\\d{4} \\d\\d:\\d\\d:\\d\\d)', '93'],
    ['klogI', '^I(?=\\d{4} \\d\\d:\\d\\d:\\d\\d)', '36'],
    ['ts', '\\b\\d{4}-\\d\\d-\\d\\d(?:[T ]\\d\\d:\\d\\d(?::\\d\\d)?(?:[.,]\\d+)?(?:Z|[+-]\\d\\d:?\\d\\d)?)?\\b|\\b\\d\\d:\\d\\d:\\d\\d(?:[.,]\\d+)?\\b', '90'],
    ['err', words(['errors?', 'err', 'fatal', 'fail', 'fails', 'failed', 'failing', 'failures?', 'panic(?:ked)?', 'exceptions?', 'critical', 'crit', 'emerg(?:ency)?',
      'denied', 'forbidden', 'unauthori[sz]ed', 'refused', 'rejected', 'unreachable', 'time[sd]? ?out', 'timeouts', 'not found', 'no such file or directory',
      'cannot', 'can\'t', 'unable', 'invalid', 'crashloopbackoff', 'oomkilled', 'oom', 'evicted', 'imagepullbackoff', 'errimagepull', 'errimageneverpull',
      'createcontainerconfigerror', 'createcontainererror', 'invalidimagename', 'killed', 'kill(?:ing)?', 'abort(?:ed)?', 'segmentation fault', 'core dumped',
      'notready', 'unhealthy', 'lost', 'backoff', 'missing', 'broken', 'corrupt(?:ed)?']), '1;91'],
    ['warn', words(['warn', 'warning', 'warnings', 'deprecated', 'deprecation', 'pending', 'retry', 'retrying', 'retries', 'terminating', 'terminated',
      'containercreating', 'podinitializing', 'init:\\d+/\\d+', 'waiting', 'degraded', 'throttl(?:ed|ing)', 'skipped', 'skipping', 'unknown', 'released',
      'deleted', 'deleting', 'restarting', 'restarted', 'paused', 'suspended', 'cordoned', 'schedulingdisabled', 'outdated', 'expired', 'expiring']), '93'],
    ['ok', words(['ok', 'okay', 'success', 'successful', 'successfully', 'succeeded', 'succeed', 'done', 'passed', 'complete', 'completed', 'running', 'ready',
      'healthy', 'active', 'enabled', 'connected', 'established', 'listening', 'accepted', 'created', 'configured', 'unchanged', 'bound', 'available',
      'synced', 'started', 'allowed', 'valid', 'verified', 'online']), '92'],
    ['info', words(['info', 'notice', 'note']), '36'],
    ['dbg', words(['debug', 'trace', 'verbose']), '90'],
  ];
  const names = RULES.map(r => r[0]);
  const RE = new RegExp(RULES.map(([n, src]) => `(?<${n}>${src})`).join('|'), 'gim');
  const CODE = Object.fromEntries(RULES.map(([n, , c]) => [n, c]));

  function colour(text) {
    return text.replace(RE, (...a) => {
      const g = a[a.length - 1];
      for (const n of names) if (g[n] !== undefined) return `\x1b[${CODE[n]}m${g[n]}\x1b[0m`;
      return a[0];
    });
  }

  // SGR tracking: which attributes the program has switched on.
  function applySGR(attrs, params) {
    const ps = params === '' ? ['0'] : params.split(';');
    for (let i = 0; i < ps.length; i++) {
      const p = ps[i].split(':')[0];
      const n = p === '' ? 0 : +p;
      if (n === 0) attrs.clear();
      else if (n >= 1 && n <= 9) attrs.add('a' + n);
      else if (n === 21 || n === 22) { attrs.delete('a1'); attrs.delete('a2'); }
      else if (n >= 23 && n <= 29) { attrs.delete('a' + (n - 20)); if (n === 25) attrs.delete('a6'); }
      else if ((n >= 30 && n <= 37) || (n >= 90 && n <= 97)) attrs.add('fg');
      else if (n === 39) attrs.delete('fg');
      else if ((n >= 40 && n <= 47) || (n >= 100 && n <= 107)) attrs.add('bg');
      else if (n === 49) attrs.delete('bg');
      else if (n === 38 || n === 48) {
        attrs.add(n === 38 ? 'fg' : 'bg');
        if (!ps[i].includes(':')) i += ps[i + 1] === '5' ? 2 : ps[i + 1] === '2' ? 4 : 0;
      }
    }
  }

  // Returns the index just past the escape sequence at s[i] (s[i] === ESC), or -1 if it is cut off.
  function escEnd(s, i) {
    if (i + 1 >= s.length) return -1;
    const c = s[i + 1];
    if (c === '[') {
      for (let j = i + 2; j < s.length; j++) { const k = s.charCodeAt(j); if (k >= 0x40 && k <= 0x7e) return j + 1; }
      return -1;
    }
    if (c === ']' || c === 'P' || c === '_' || c === '^' || c === 'X') { // OSC/DCS…: until BEL or ESC \
      for (let j = i + 2; j < s.length; j++) {
        if (s[j] === '\x07') return j + 1;
        if (s[j] === '\x1b') { if (j + 1 >= s.length) return -1; if (s[j + 1] === '\\') return j + 2; }
      }
      return -1;
    }
    if (c === '(' || c === ')' || c === '*' || c === '+' || c === '#' || c === '%') return i + 3 <= s.length ? i + 3 : -1;
    return i + 2;
  }

  function create() {
    const attrs = new Set();
    let alt = false, pending = '';
    const plain = () => attrs.size === 0 && !alt;

    // process(text, final): returns what to write now; may keep a cut-off tail for the next call.
    function process(input, final = false) {
      const s = pending + input;
      pending = '';
      let out = '', run = '', i = 0;
      const flushRun = () => { if (run) { out += plain() ? colour(run) : run; run = ''; } };
      while (i < s.length) {
        const e = s.indexOf('\x1b', i);
        if (e < 0) { run += s.slice(i); break; }
        run += s.slice(i, e);
        const end = escEnd(s, e);
        if (end < 0) {
          if (final) { flushRun(); out += s.slice(e); return out; }
          flushRun(); pending = s.slice(e); return out;
        }
        flushRun();
        const seq = s.slice(e, end);
        out += seq;
        if (seq[1] === '[') {
          const fin = seq[seq.length - 1], body = seq.slice(2, -1);
          if (fin === 'm' && /^[\d;:]*$/.test(body)) applySGR(attrs, body);
          else if ((fin === 'h' || fin === 'l') && /^\?(?:1049|1047|47)$/.test(body)) alt = fin === 'h';
        } else if (seq === '\x1bc') { attrs.clear(); alt = false; }
        i = end;
      }
      // keep a word that may continue in the next chunk, so "err" + "or" still becomes "error"
      if (!final && run && plain() && s.length >= 16) {
        const m = /[^\s]{1,100}$/.exec(run);
        if (m && m.index > 0) { pending = run.slice(m.index); run = run.slice(0, m.index); }
      }
      flushRun();
      return out;
    }
    return {
      write: text => process(text, false),
      flush: () => process('', true),
      hasPending: () => pending !== '',
      reset: () => { attrs.clear(); alt = false; pending = ''; },
    };
  }

  return { create, colour, RULES };
})();

if (typeof module !== 'undefined') module.exports = TermHL;
