import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ProxyService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import App from './App.jsx';

vi.mock('../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch', () => ({
  StringService: { Process: vi.fn().mockResolvedValue({ result: '', metadata: { success: true, operation: 'avro-to-plain', message: 'Done' } }) },
  RegistryService: { Fetch: vi.fn(), Schema: vi.fn() },
  ProfileService: { List: vi.fn(), Apply: vi.fn(), Save: vi.fn(), Delete: vi.fn(), SaveDefault: vi.fn() },
  ProxyService: { ListConfigs: vi.fn(), SaveDraft: vi.fn(), SaveConfig: vi.fn(), DeleteConfig: vi.fn(), Start: vi.fn(), Stop: vi.fn(), Status: vi.fn(), Console: vi.fn(), ClearConsole: vi.fn(),
    SetDebug: vi.fn(), SetActivityLimit: vi.fn(), ClearActivity: vi.fn(), Capture: vi.fn(), CopyCapture: vi.fn(), CopyConsole: vi.fn(), SetConsoleLimit: vi.fn() },
}));
vi.mock('@wailsio/runtime', () => ({ Clipboard: { SetText: vi.fn() } }));
Object.defineProperty(window, 'matchMedia', { writable: true, value: vi.fn().mockImplementation(() => ({ matches: false, addEventListener() {}, removeEventListener() {} })) });
globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
globalThis.IS_REACT_ACT_ENVIRONMENT = true;

let container, root, goState, drafts;
const stopped = { success: true, message: '', running: false, address: '', target: '', requests: 0, failures: 0, lastError: '', activity: [] };
const tab = (name) => [...container.querySelectorAll('[role="tab"]')].find((e) => e.textContent === name);
const field = (name) => container.querySelector(`input[aria-label="${name}"]`);
const button = (name) => [...container.querySelectorAll('button')].find((e) => e.textContent.trim() === name);
const script = () => container.querySelector('#proxy-script-editor');
const serverState = () => container.querySelector('[aria-label="Proxy server state"]').textContent;
const statusText = () => container.querySelector('.proxy-status').textContent;
const flush = () => act(async () => { await Promise.resolve(); await Promise.resolve(); });
async function click(e) { await act(async () => e.click()); }
async function edit(element, value) {
  await act(async () => {
    const proto = element instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(element, value);
    element.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
async function mount() { root = createRoot(container); await act(async () => root.render(<App />)); }
async function openProxy() { await click(tab('Dynamic Proxy')); await flush(); }
function storeResponse(patch = {}) { return { success: true, message: '', draft: null, saved: [], ...patch }; }

beforeEach(async () => {
  window.localStorage.clear();
  goState = { ...stopped };
  drafts = [];
  for (const fn of Object.values(ProxyService)) fn.mockReset();
  ProxyService.ListConfigs.mockResolvedValue(storeResponse());
  ProxyService.SaveDraft.mockImplementation(async (config) => { drafts.push(config); return storeResponse({ message: 'Draft saved.', draft: config }); });
  ProxyService.Status.mockImplementation(async () => ({ ...goState }));
  ProxyService.Start.mockImplementation(async (config) => {
    goState = { ...stopped, running: true, address: `http://127.0.0.1:${config.listenPort}`, target: config.targetURL };
    return { ...goState, message: 'Proxy running.' };
  });
  ProxyService.Stop.mockImplementation(async () => { goState = { ...stopped }; return { ...goState, message: 'Proxy stopped.' }; });
  container = document.createElement('div'); document.body.append(container); await mount();
});
afterEach(async () => { await act(async () => root.unmount()); container.remove(); vi.useRealTimers(); });

it('adds a Dynamic Proxy tab that starts stopped with defaults and collapsed Path Mapping', async () => {
  await openProxy();
  expect(serverState()).toBe('Stopped');
  expect(button('Start')).toBeDefined();
  expect(field('Local Port').value).toBe('8080');
  expect(script().value).toContain('function onRequest(req)');
  const mapping = container.querySelector('[aria-controls="proxy-mapping"]');
  expect(mapping.getAttribute('aria-expanded')).toBe('false');
  expect(container.querySelector('#proxy-mapping').style.display).toBe('none');
  expect(container.textContent).toContain('Off — paths forwarded unchanged');
  await click(mapping);
  expect(container.querySelector('#proxy-mapping').style.display).not.toBe('none');
  await edit(field('Source Path'), '/api'); await edit(field('Target Path'), '/v2');
  expect(container.querySelector('[aria-label="Path mapping status"]').textContent).toBe('/api → /v2');
  expect(window.localStorage.getItem('toolhatch.active-tab')).toBe('dynamic-proxy');
  expect(JSON.stringify(window.localStorage)).not.toContain('onRequest');
});

it('restores the saved draft and starts/stops with the Go-reported state', async () => {
  ProxyService.ListConfigs.mockResolvedValue(storeResponse({ draft: { listenPort: 9100, targetURL: 'https://api.example.com', sourcePath: '', targetPath: '', script: '' } }));
  await act(async () => root.unmount()); await mount(); await openProxy();
  expect(field('Local Port').value).toBe('9100'); expect(field('Target URL').value).toBe('https://api.example.com'); expect(script().value).toBe('');
  await click(button('Start')); await flush();
  expect(ProxyService.Start).toHaveBeenCalledWith({ listenPort: 9100, targetURL: 'https://api.example.com', sourcePath: '', targetPath: '', script: '' });
  expect(serverState()).toBe('Running');
  expect(statusText()).toContain('http://127.0.0.1:9100 → https://api.example.com');
  await click(button('Stop')); await flush();
  expect(ProxyService.Stop).toHaveBeenCalledTimes(1);
  expect(serverState()).toBe('Stopped'); expect(button('Start')).toBeDefined();
});

it('shows validation and startup failures from Go without claiming the proxy runs', async () => {
  ProxyService.Start.mockResolvedValue({ ...stopped, success: false, message: 'Port 8080 is already in use on 127.0.0.1.' });
  await openProxy();
  await click(button('Start')); await flush();
  expect(serverState()).toBe('Stopped');
  expect(statusText()).toContain('already in use');
  expect(container.querySelector('.proxy-status-text').getAttribute('style') ?? '').toContain('red');
  ProxyService.Start.mockRejectedValue(new Error('bridge'));
  await click(button('Start')); await flush();
  expect(statusText()).toContain('Could not contact the Go backend');
});

it('polls actual server state, ignores a poll that was issued before Stop, and keeps running across tabs', async () => {
  goState = { ...stopped, running: true, address: 'http://127.0.0.1:8080', target: 'http://localhost:3000', requests: 2, failures: 1,
    activity: [{ id: 1, time: '', method: 'GET', path: '/ok', status: 200, durationMs: 4, error: '' },
      { id: 2, time: '', method: 'POST', path: '/chat', status: 500, durationMs: 9, error: 'Script error: denied' }] };
  await openProxy();
  expect(serverState()).toBe('Running');
  expect(container.querySelector('[aria-label="Request count"]').textContent).toBe('2 req');
  const rows = [...container.querySelectorAll('.activity-entry')].map((row) => row.textContent);
  expect(rows[0]).toContain('POST/chat'); expect(rows[0]).toContain('Script error: denied'); expect(rows[1]).toContain('GET/ok');

  let resolvePoll;
  ProxyService.Status.mockImplementationOnce(() => new Promise((resolve) => { resolvePoll = resolve; }));
  await click(tab('String')); await click(tab('Dynamic Proxy')); // showing the tab polls immediately
  expect(typeof resolvePoll).toBe('function');
  await click(button('Stop')); await flush();
  expect(serverState()).toBe('Stopped');
  await act(async () => resolvePoll({ ...stopped, running: true, address: 'http://127.0.0.1:8080', target: 'x' }));
  expect(serverState()).toBe('Stopped');

  goState = { ...stopped, running: true, address: 'http://127.0.0.1:8080', target: 'x' };
  await click(tab('String'));
  expect(ProxyService.Stop).toHaveBeenCalledTimes(1);
  const callsWhileHidden = ProxyService.Status.mock.calls.length;
  await click(tab('Dynamic Proxy')); await flush();
  expect(ProxyService.Status.mock.calls.length).toBeGreaterThan(callsWhileHidden);
  expect(serverState()).toBe('Running');
});

it('marks edits made while running as needing a restart', async () => {
  await openProxy();
  await edit(field('Target URL'), 'http://localhost:3000');
  await click(button('Start')); await flush();
  expect(container.textContent).not.toContain('Changes apply after restart');
  await edit(script(), 'function onRequest(req) { req.setHeader("X-A", "1"); }');
  expect(container.textContent).toContain('Changes apply after restart');
});

it('autosaves the draft serially with the latest snapshot and never before restore succeeds', async () => {
  await openProxy();
  let release;
  ProxyService.SaveDraft.mockImplementationOnce((config) => new Promise((resolve) => { drafts.push(config); release = () => resolve(storeResponse({ message: 'Draft saved.' })); }));
  await edit(field('Target URL'), 'http://a');
  await edit(field('Target URL'), 'http://ab');
  await edit(field('Target URL'), 'http://abc');
  expect(drafts).toHaveLength(1);
  await act(async () => release()); await flush();
  expect(drafts.map((d) => d.targetURL)).toEqual(['http://a', 'http://abc']);
  expect(container.querySelector('[aria-label="Proxy storage status"]').textContent).toBe('Draft saved.');

  ProxyService.ListConfigs.mockResolvedValue({ success: false, message: 'Invalid proxy configuration storage.', draft: null, saved: [] });
  ProxyService.SaveDraft.mockClear();
  await act(async () => root.unmount()); await mount(); await openProxy();
  expect(container.querySelector('[aria-label="Proxy storage status"]').textContent).toContain('Invalid proxy configuration storage');
  await edit(field('Target URL'), 'http://never-saved');
  expect(ProxyService.SaveDraft).not.toHaveBeenCalled();
});

it('saves, loads with unsaved-draft confirmation, and deletes named configurations', async () => {
  const savedConfig = { listenPort: 7000, targetURL: 'http://saved', sourcePath: '/a', targetPath: '/b', script: '' };
  ProxyService.SaveConfig.mockImplementation(async (name, config) => storeResponse({ message: `Saved "${name}".`, saved: [{ name, config }] }));
  ProxyService.DeleteConfig.mockResolvedValue(storeResponse({ message: 'Deleted "Local".', saved: [] }));
  ProxyService.ListConfigs.mockResolvedValue(storeResponse({ saved: [{ name: 'Local', config: savedConfig }] }));
  await act(async () => root.unmount()); await mount(); await openProxy();
  await click(container.querySelector('[aria-controls="proxy-saved"]'));
  const select = container.querySelector('select[aria-label="Saved configuration"]');
  await act(async () => { select.value = 'Local'; select.dispatchEvent(new Event('change', { bubbles: true })); });
  await click(button('Load'));
  expect(document.body.textContent).toContain('Replace unsaved draft?');
  await click([...document.querySelectorAll('button')].find((e) => e.textContent === 'Replace draft'));
  expect(field('Local Port').value).toBe('7000'); expect(field('Source Path').value).toBe('/a');
  expect(drafts.at(-1)).toEqual(savedConfig);

  await edit(field('Configuration Name'), 'Local');
  await edit(field('Target URL'), 'http://changed');
  await click(button('Save')); await flush();
  expect(ProxyService.SaveConfig).toHaveBeenCalledWith('Local', { ...savedConfig, targetURL: 'http://changed' });
  expect(container.querySelector('[aria-label="Proxy storage status"]').textContent).toBe('Saved "Local".');
  await click(button('Delete')); await flush();
  expect(ProxyService.DeleteConfig).toHaveBeenCalledWith('Local');
  expect(container.querySelector('select[aria-label="Saved configuration"]').textContent).toContain('No saved configurations');
});

it('applies a slow Start response even when a poll overlapping it resolves first', async () => {
  await openProxy();
  let finishStart;
  ProxyService.Start.mockImplementationOnce((config) => new Promise((resolve) => { finishStart = () => resolve({ ...stopped, success: true, message: 'Proxy running.', running: true, address: `http://127.0.0.1:${config.listenPort}`, target: 't' }); }));
  await click(button('Start'));
  await click(tab('String')); await click(tab('Dynamic Proxy')); await flush(); // poll resolves "stopped" mid-Start
  expect(button('Start').disabled).toBe(true);
  await act(async () => finishStart()); await flush();
  expect(serverState()).toBe('Running');
  expect(button('Stop')).toBeDefined();
});

const logEntry = (id, level, message) => ({ id, time: `2026-11-15T10:20:3${id}.12${id}-06:00`, level, message });
const consoleButton = () => container.querySelector('[aria-controls="proxy-console-output"]');
const consoleLines = () => [...container.querySelectorAll('.console-entry')].map((row) => row.textContent);
async function repoll() { await click(tab('String')); await click(tab('Dynamic Proxy')); await flush(); }
function consoleBackend(entries) {
  ProxyService.Console.mockImplementation(async (after) => ({ entries: entries.filter((e) => e.id > after), lastID: entries.at(-1)?.id ?? 0, truncated: false }));
}

it('places a collapsed Console below Activity and fetches entries only when the Go cursor advances', async () => {
  const entries = [logEntry(1, 'log', 'path /x 42'), logEntry(2, 'warn', 'careful'), logEntry(3, 'error', 'Error: bad')];
  consoleBackend(entries);
  await openProxy();
  const side = container.querySelector('.proxy-side');
  expect([...side.children].map((e) => e.getAttribute('aria-label'))).toEqual(['Request activity', 'Request inspector', 'Script console']);
  expect(consoleButton().getAttribute('aria-expanded')).toBe('false');
  expect(container.querySelector('#proxy-console-output')).toBeNull();
  expect(ProxyService.Console).not.toHaveBeenCalled(); // cursor 0: nothing to fetch

  goState = { ...goState, consoleLastID: 3 };
  await repoll();
  expect(ProxyService.Console).toHaveBeenCalledTimes(1);
  expect(ProxyService.Console).toHaveBeenLastCalledWith(0);
  expect(container.querySelector('[aria-label="Console entry count"]').textContent).toBe('3');
  await click(consoleButton());
  expect(consoleLines()).toEqual(['10:20:31.121logpath /x 42', '10:20:32.122warncareful', '10:20:33.123errorError: bad']);

  await repoll(); await repoll(); // unchanged cursor: no extra Console calls
  expect(ProxyService.Console).toHaveBeenCalledTimes(1);
  entries.push(logEntry(4, 'debug', 'later'));
  goState = { ...goState, consoleLastID: 4 };
  await repoll();
  expect(ProxyService.Console).toHaveBeenLastCalledWith(3);
  expect(consoleLines().at(-1)).toContain('debuglater');
});

it('keeps Console independent of Activity and Start/Stop when collapsing, expanding and clearing', async () => {
  const entries = [logEntry(1, 'info', 'loaded')];
  consoleBackend(entries);
  ProxyService.ClearConsole.mockResolvedValue({ entries: [], lastID: 1, truncated: false });
  goState = { ...stopped, running: true, address: 'http://127.0.0.1:8080', target: 't', requests: 1, consoleLastID: 1,
    activity: [{ id: 1, time: '', method: 'GET', path: '/ok', status: 200, durationMs: 1, error: '' }] };
  await openProxy();
  await click(consoleButton());
  expect(container.querySelector('.proxy-console').getAttribute('data-open')).toBe('true');
  await click(consoleButton());
  expect(container.querySelector('.proxy-console').getAttribute('data-open')).toBe('false');
  expect(container.querySelector('#proxy-console-output')).toBeNull();
  expect(ProxyService.Stop).not.toHaveBeenCalled(); expect(ProxyService.Start).not.toHaveBeenCalled();
  expect(serverState()).toBe('Running');
  expect(container.querySelectorAll('.activity-entry')).toHaveLength(1);

  // Logging continues while collapsed.
  entries.push(logEntry(2, 'log', 'while collapsed'));
  goState = { ...goState, consoleLastID: 2 };
  await repoll();
  expect(container.querySelector('[aria-label="Console entry count"]').textContent).toBe('2');

  // A fetch still in flight when Clear is pressed must not repopulate the view.
  let late;
  ProxyService.Console.mockImplementationOnce(() => new Promise((resolve) => { late = resolve; }));
  entries.push(logEntry(3, 'log', 'raced'));
  goState = { ...goState, consoleLastID: 3 };
  await repoll();
  await click(consoleButton());
  await click(button('Clear')); await flush();
  expect(ProxyService.ClearConsole).toHaveBeenCalledTimes(1);
  await act(async () => late({ entries: [entries[2]], lastID: 3, truncated: false }));
  expect(consoleLines()).toEqual([]);
  expect(container.querySelector('[aria-label="Console entry count"]').textContent).toBe('0');
  expect(container.querySelectorAll('.activity-entry')).toHaveLength(1);
  expect(ProxyService.Stop).not.toHaveBeenCalled();
});

it('reports discarded console entries', async () => {
  ProxyService.Console.mockResolvedValue({ entries: [logEntry(9, 'log', 'tail')], lastID: 9, truncated: true });
  goState = { ...goState, consoleLastID: 9 };
  await openProxy();
  await click(consoleButton());
  expect(container.querySelector('#proxy-console-output').textContent).toContain('Older entries were discarded');
  expect(consoleLines()).toEqual(['10:20:39.129logtail']);
});

const findInput = () => container.querySelector('input[aria-label="Find in Proxy Script"]');
const findCount = () => container.querySelector('.editor-find-count').textContent;
const findModifier = () => (/Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || '') ? { metaKey: true } : { ctrlKey: true });
async function key(element, init) {
  let event;
  await act(async () => { event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init }); element.dispatchEvent(event); });
  return event;
}

it('finds literal text in the Proxy Script with Cmd/Ctrl+F, Enter/Shift+Enter wrapping and Escape', async () => {
  await openProxy();
  const text = 'abc foo\nFoo bar\nfoo end';
  await edit(script(), text);
  const saves = ProxyService.SaveDraft.mock.calls.length;
  expect(container.querySelector('label[for="proxy-script-editor"]').textContent).toBe('Proxy Script');
  script().focus(); script().setSelectionRange(0, 0);

  // Shortcuts outside the editor are not intercepted.
  const outside = await key(field('Target URL'), { key: 'f', ...findModifier() });
  expect(outside.defaultPrevented).toBe(false);
  expect(findInput()).toBeNull();

  const opened = await key(script(), { key: 'f', ...findModifier() });
  expect(opened.defaultPrevented).toBe(true);
  expect(document.activeElement).toBe(findInput());
  await edit(findInput(), 'foo');
  expect(findCount()).toBe('1/3');
  expect([script().selectionStart, script().selectionEnd]).toEqual([4, 7]);
  await key(findInput(), { key: 'Enter' });
  expect(findCount()).toBe('2/3'); expect(script().selectionStart).toBe(8);
  await key(findInput(), { key: 'Enter' });
  expect(findCount()).toBe('3/3'); expect(script().selectionStart).toBe(16);
  await key(findInput(), { key: 'Enter' });
  expect(findCount()).toBe('1/3'); expect(script().selectionStart).toBe(4); // wrapped to the first
  await key(findInput(), { key: 'Enter', shiftKey: true });
  expect(findCount()).toBe('3/3'); expect(script().selectionStart).toBe(16); // wrapped to the last
  expect(document.activeElement).toBe(findInput());
  await key(findInput(), { key: 'Enter', isComposing: true }); // IME composition is left alone
  expect(findCount()).toBe('3/3');

  await edit(findInput(), 'zzz');
  expect(findCount()).toBe('No results');
  await key(findInput(), { key: 'Enter' });
  expect(script().selectionStart).toBe(16);

  await edit(findInput(), 'bar');
  await key(findInput(), { key: 'Escape' });
  expect(findInput()).toBeNull();
  expect(document.activeElement).toBe(script());
  expect([script().selectionStart, script().selectionEnd]).toEqual([12, 15]);
  expect(script().value).toBe(text);
  expect(ProxyService.SaveDraft.mock.calls.length).toBe(saves); // searching never edits the draft

  // Reopening prefills the current single-line selection.
  await key(script(), { key: 'f', ...findModifier() });
  expect(findInput().value).toBe('bar'); expect(findCount()).toBe('1/1');

  // Other uses of the shared Editor do not intercept the shortcut.
  await click(tab('String'));
  const elsewhere = await key(container.querySelector('#input-editor'), { key: 'f', ...findModifier() });
  expect(elsewhere.defaultPrevented).toBe(false);
  expect(container.querySelector('input[aria-label^="Find in"]:not([aria-label="Find in Proxy Script"])')).toBeNull();
});

const activityEntry = (id, patch = {}) => ({ id, time: '2026-11-17T10:00:00-06:00', method: 'POST', path: `/v1/r${id}`, status: 200, durationMs: 5, error: '', pending: false, debug: false, ...patch });
const part = (preview, patch = {}) => ({ preview, truncated: false, size: preview.length, observed: preview.length, state: 'complete', binary: false, note: '', ...patch });
function captureView(id, body) {
  return { success: true, message: '', entry: activityEntry(id), state: 'complete', note: '',
    request: { line: `POST /v1/r${id}`, headers: part('Authorization: •••• (20 characters hidden; Copy includes it)'), body: part(body, { truncated: true, size: 1 << 20 }) },
    response: { line: '200 OK', headers: part('Content-Type: application/json'), body: part('{"ok":true}') } };
}
const rows = () => [...container.querySelectorAll('.activity-entry')];

it('toggles Debug through Go and inspects only selected DEBUG entries with Copy via Go', async () => {
  goState = { ...stopped, running: true, address: 'http://127.0.0.1:8080', target: 't', requests: 2, debug: false, debugBytes: 2048, debugLimitBytes: 200 << 20, activityLimit: 200,
    activity: [activityEntry(1), activityEntry(2, { debug: true, pending: true, status: 0 }), activityEntry(3, { debug: true })] };
  ProxyService.SetDebug.mockImplementation(async (enabled) => { goState = { ...goState, debug: enabled }; return { ...goState, message: 'ok' }; });
  ProxyService.SetActivityLimit.mockImplementation(async (limit) => { goState = { ...goState, activityLimit: limit }; return { ...goState }; });
  ProxyService.ClearActivity.mockImplementation(async () => { goState = { ...goState, activity: [], debugBytes: 0 }; return { ...goState }; });
  ProxyService.CopyCapture.mockResolvedValue({ success: true, message: 'Copied 1.0 MiB.', bytes: 1 << 20, base64: false, complete: true });
  const pendingCaptures = {};
  ProxyService.Capture.mockImplementation((id) => new Promise((resolve) => { pendingCaptures[id] = () => resolve(captureView(id, `body-${id}`)); }));
  await openProxy();

  const debugSwitch = container.querySelector('input[aria-label="Debug capture"]');
  expect(debugSwitch.checked).toBe(false);
  await click(debugSwitch); await flush();
  expect(ProxyService.SetDebug).toHaveBeenCalledWith(true);
  expect(container.querySelector('input[aria-label="Debug capture"]').checked).toBe(true);

  // Newest first: #3, #2 (pending, DEBUG), #1 (not captured, not selectable).
  expect(rows().map((row) => row.getAttribute('role'))).toEqual(['button', 'button', null]);
  expect(rows()[1].textContent).toContain('…'); expect(rows()[1].textContent).toContain('open');
  expect(rows()[0].textContent).toContain('DEBUG'); expect(rows()[2].textContent).not.toContain('DEBUG');
  const inspector = () => container.querySelector('.proxy-inspector');
  expect(inspector().getAttribute('data-open')).toBe('false');
  await click(rows()[2]);
  expect(ProxyService.Capture).not.toHaveBeenCalled();
  expect(inspector().getAttribute('data-open')).toBe('false');

  // Selecting #2 then #3: the late answer for #2 must not replace #3's view.
  await click(rows()[1]);
  expect(inspector().getAttribute('data-open')).toBe('true');
  expect(ProxyService.Capture).toHaveBeenLastCalledWith(2);
  await click(rows()[0]);
  expect(ProxyService.Capture).toHaveBeenLastCalledWith(3);
  await act(async () => pendingCaptures[3]());
  await act(async () => pendingCaptures[2]());
  expect(inspector().textContent).toContain('body-3'); expect(inspector().textContent).not.toContain('body-2');
  expect(inspector().textContent).toContain('#3 POST /v1/r3');
  expect(inspector().textContent).toContain('Preview shows the first 6 B; Copy copies all 1.0 MiB.');
  expect(inspector().textContent).toContain('•••• (20 characters hidden');
  expect(rows()[0].getAttribute('aria-pressed')).toBe('true');

  await click(container.querySelector('button[aria-label="Copy Request body"]')); await flush();
  expect(ProxyService.CopyCapture).toHaveBeenCalledWith(3, 'requestBody');
  expect(inspector().textContent).toContain('Copied 1.0 MiB.');

  const select = container.querySelector('select[aria-label="Activity retention"]');
  expect(select.value).toBe('200');
  await act(async () => { select.value = '500'; select.dispatchEvent(new Event('change', { bubbles: true })); }); await flush();
  expect(ProxyService.SetActivityLimit).toHaveBeenCalledWith(500);

  await click(container.querySelector('button[aria-label="Clear activity"]')); await flush();
  expect(ProxyService.ClearActivity).toHaveBeenCalledTimes(1);
  expect(rows()).toHaveLength(0);
  expect(inspector().textContent).toContain('This entry is no longer retained.');
  expect(ProxyService.Stop).not.toHaveBeenCalled();
});

it('refreshes a selected pending capture on existing polls even when allocated memory is unchanged', async () => {
  vi.useFakeTimers();
  goState = { ...stopped, running: true, debugBytes: 65536,
    activity: [activityEntry(1, { debug: true, pending: true, status: 0 })] };
  let body = 'first event';
  ProxyService.Capture.mockImplementation(async () => captureView(1, body));
  await openProxy();
  await click(rows()[0]);
  const inspector = () => container.querySelector('.proxy-inspector');
  expect(inspector().textContent).toContain('first event');
  body = 'first event; second event';
  const polls = ProxyService.Status.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
  expect(ProxyService.Status.mock.calls.length).toBe(polls + 1);
  expect(inspector().textContent).toContain('first event; second event');

  // Collapsed and hidden inspectors must not add capture fetches.
  await click(container.querySelector('[aria-controls="proxy-inspector-body"]'));
  const collapsedCalls = ProxyService.Capture.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(ProxyService.Capture).toHaveBeenCalledTimes(collapsedCalls);
  await click(tab('String'));
  const hiddenCalls = ProxyService.Status.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(ProxyService.Status).toHaveBeenCalledTimes(hiddenCalls);

  await click(tab('Dynamic Proxy'));
  await click(container.querySelector('[aria-controls="proxy-inspector-body"]'));
  const openCalls = ProxyService.Capture.mock.calls.length;
  await click(tab('String'));
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(ProxyService.Capture).toHaveBeenCalledTimes(openCalls);
  await click(tab('Dynamic Proxy'));
  goState = { ...goState, activity: [activityEntry(1, { debug: true })] };
  await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
  const completedCalls = ProxyService.Capture.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(ProxyService.Capture).toHaveBeenCalledTimes(completedCalls);
});

it('shows Console previews and copies full messages by entry ID through Go', async () => {
  const long = { ...logEntry(1, 'log', 'x'.repeat(2048)), size: 100000, previewTruncated: true, incomplete: false };
  consoleBackend([long, logEntry(2, 'info', 'short')]);
  ProxyService.CopyConsole.mockResolvedValue({ success: true, message: 'Copied 97.7 KiB.', bytes: 100000, base64: false, complete: true });
  ProxyService.SetConsoleLimit.mockResolvedValue({ entries: [], lastID: 2, truncated: false, limit: 100 });
  goState = { ...goState, consoleLastID: 2, consoleLimit: 500 };
  await openProxy();
  await click(consoleButton());
  const entries = [...container.querySelectorAll('.console-entry')];
  expect(entries[0].textContent).toContain('preview of 97.7 KiB');
  expect(entries[1].textContent).not.toContain('preview of');
  await click(container.querySelector('button[aria-label="Copy console entry 1"]')); await flush();
  expect(ProxyService.CopyConsole).toHaveBeenCalledWith(1);
  expect(container.querySelector('.proxy-console').textContent).toContain('Copied 97.7 KiB.');
  const select = container.querySelector('select[aria-label="Console retention"]');
  expect(select.value).toBe('500');
  await act(async () => { select.value = '100'; select.dispatchEvent(new Event('change', { bubbles: true })); }); await flush();
  expect(ProxyService.SetConsoleLimit).toHaveBeenCalledWith(100);
});
