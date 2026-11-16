import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ProxyService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import App from './App.jsx';

vi.mock('../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch', () => ({
  StringService: { Process: vi.fn().mockResolvedValue({ result: '', metadata: { success: true, operation: 'avro-to-plain', message: 'Done' } }) },
  RegistryService: { Fetch: vi.fn(), Schema: vi.fn() },
  ProfileService: { List: vi.fn(), Apply: vi.fn(), Save: vi.fn(), Delete: vi.fn(), SaveDefault: vi.fn() },
  ProxyService: { ListConfigs: vi.fn(), SaveDraft: vi.fn(), SaveConfig: vi.fn(), DeleteConfig: vi.fn(), Start: vi.fn(), Stop: vi.fn(), Status: vi.fn() },
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
const script = () => container.querySelector('#onrequest-script-editor');
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
