import { act } from 'react';
import { createRoot } from 'react-dom/client';
import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { RegistryService, ProfileService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import { Clipboard } from '@wailsio/runtime';
import App from './App.jsx';
vi.mock('../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch', () => ({
  RegistryService: { Fetch: vi.fn(), Schema: vi.fn() },
  ProfileService: { List: vi.fn().mockResolvedValue({ success: true, message: '', profiles: [], selectedID: '', config: null, credentialAvailable: true }), Apply: vi.fn(), Save: vi.fn(), Delete: vi.fn(), SaveDefault: vi.fn() },
  StringService: { Process: vi.fn().mockResolvedValue({ result: '', metadata: { success: true, operation: 'avro-to-plain', message: 'Done' } }) },
}));
vi.mock('@wailsio/runtime', () => ({ Clipboard: { SetText: vi.fn() } }));
Object.defineProperty(window, 'matchMedia', { writable: true, value: vi.fn().mockImplementation(() => ({ matches: false, addEventListener() {}, removeEventListener() {} })) });
globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
globalThis.IS_REACT_ACT_ENVIRONMENT = true;
let container, root, requests, defaultState;
const tab = (name) => [...container.querySelectorAll('[role="tab"]')].find((e) => e.textContent === name);
const field = (name) => container.querySelector(`input[aria-label="${name}"]`);
const status = () => container.querySelector('.registry-status').textContent;
const schema = () => container.querySelector('textarea[readonly]');
const button = (name) => [...container.querySelectorAll('button')].find((e) => e.textContent === name);
async function click(e) {
 const disclosure = container.querySelector('[aria-controls="registry-profiles"]');
 if (e.closest('#registry-profiles') && disclosure?.getAttribute('aria-expanded') === 'false') await act(async () => disclosure.click());
 await act(async () => e.click());
}
async function edit(name, value) {
 await act(async () => { const e = field(name); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(e, value); e.dispatchEvent(new Event('input', { bubbles: true })); });
}
async function submit() { await act(async () => container.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))); }
async function reply(index, patch = {}) { await act(async () => requests[index].resolve({ success: true, message: 'Schema loaded.', schema: '{\n  "type": "string"\n}', versions: [], version: 3, ...patch })); }
async function mount() { root = createRoot(container); await act(async () => root.render(<App />)); }
async function registry() { await click(tab('Schema Registry')); }
async function config() { await edit('Schema Registry URL', 'https://registry.example.com'); await edit('Topic Name', 'orders'); }
beforeEach(async () => {
 window.localStorage.clear(); requests = [];
 ProfileService.List.mockReset().mockResolvedValue({ success: true, message: '', profiles: [], selectedID: '', config: null, credentialAvailable: true });
 for (const name of ['Apply', 'Save', 'Delete', 'SaveDefault']) ProfileService[name].mockReset();
 defaultState = { url: '', keystorePath: '', truststorePath: '', password: '' };
 ProfileService.Apply.mockImplementation(async () => ({ success: true, warning: false, message: 'Default Profile restored.', profiles: [], selectedID: '', config: { ...defaultState }, credentialAvailable: true }));
 ProfileService.SaveDefault.mockImplementation(async (connection) => { defaultState = { ...connection }; return { success: true, warning: false, message: 'Default Profile saved automatically.', profiles: [], selectedID: '', config: null, credentialAvailable: true }; });
 for (const method of ['Fetch', 'Schema']) RegistryService[method].mockReset().mockImplementation((config, version) => new Promise((resolve, reject) => requests.push({ config, version, method, resolve, reject })));
 Clipboard.SetText.mockReset().mockResolvedValue(undefined);
 container = document.createElement('div'); document.body.append(container); await mount();
});
afterEach(async () => { await act(async () => root.unmount()); container.remove(); vi.restoreAllMocks(); });

it('defaults to String and restores a stable saved tab ID after remount without saving configuration', async () => {
 expect(tab('String').getAttribute('aria-selected')).toBe('true');
 await registry(); await config();
 await click(container.querySelector('[aria-controls="registry-ssl"]'));
 await edit('Password', 'private-password');
 expect(window.localStorage.getItem('toolhatch.active-tab')).toBe('schema-registry');
 expect(JSON.stringify(window.localStorage)).not.toContain('private-password');
 expect(JSON.stringify(window.localStorage)).not.toContain('registry.example.com');
 await act(async () => root.unmount()); await mount();
 expect(tab('Schema Registry').getAttribute('aria-selected')).toBe('true');
 expect(field('Schema Registry URL').value).toBe('https://registry.example.com'); expect(field('Password').value).toBe('private-password');
 await click(tab('String')); expect(window.localStorage.getItem('toolhatch.active-tab')).toBe('string');
});
it('falls back to String for invalid saved IDs and handles preference storage failures', async () => {
 await act(async () => root.unmount()); window.localStorage.setItem('toolhatch.active-tab', '42'); await mount();
 expect(tab('String').getAttribute('aria-selected')).toBe('true');
 await act(async () => root.unmount());
 vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('unavailable'); });
 vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('unavailable'); });
 await mount(); await registry(); expect(field('Schema Registry URL')).not.toBeNull();
});
it('starts with optional SSL collapsed and reports exactly the missing fields', async () => {
 await registry(); const toggle = container.querySelector('[aria-controls="registry-ssl"]');
 expect(toggle.getAttribute('aria-expanded')).toBe('false'); expect(container.querySelector('#registry-ssl').style.display).toBe('none'); expect(container.textContent).toContain('Not configured (optional)');
 await submit(); expect(status()).toContain('Schema Registry URL is required'); expect(requests).toHaveLength(0);
 await config(); await click(toggle); expect(toggle.getAttribute('aria-expanded')).toBe('true'); expect(container.querySelector('#registry-ssl').style.display).not.toBe('none');
 await edit('Keystore Path', '/local/client.p12');
 expect(container.querySelector('[aria-label="SSL configuration status"]').textContent).toBe('Missing: Truststore Path, Password');
 await click(button('Fetch')); expect(requests).toHaveLength(0); expect(status()).toContain('Truststore Path, Password');
 await edit('Truststore Path', '/local/trust.jks'); await edit('Password', 'secret');
 expect(container.textContent).toContain('Configured (not verified)'); await submit();
 expect(requests[0].config).toEqual({ url: 'https://registry.example.com', topic: 'orders', keystorePath: '/local/client.p12', truststorePath: '/local/trust.jks', password: 'secret' });
});
it('fetches on form submit (Enter), selects latest, switches versions and copies the read-only schema', async () => {
 await registry(); await config(); await submit(); expect(status()).toBe('Fetching versions…');
 await reply(0, { versions: [3, 2, 1] });
 const versions = [...container.querySelectorAll('nav button')]; expect(versions.map((e) => e.textContent)).toEqual(['v3', 'v2', 'v1']);
 expect(button('v3').getAttribute('aria-pressed')).toBe('true'); expect(schema().readOnly).toBe(true);
 await click(container.querySelector('[aria-label="Copy Avro Schema"]')); expect(Clipboard.SetText).toHaveBeenCalledWith(schema().value);
 await click(button('v1')); expect(requests[1].version).toBe(1); expect(schema().value).toBe('');
 await reply(1, { version: 1, schema: '"string"' }); expect(schema().value).toBe('"string"');
});
it('keeps the version list and its scroll position when selecting from many versions', async () => {
 await registry(); await config(); await submit();
 await reply(0, { versions: Array.from({ length: 150 }, (_,index) => 150 - index), version: 150 });
 const list = container.querySelector('[aria-label="Version list"]');
 const heading = container.querySelector('.versions-heading');
 expect(list.contains(heading)).toBe(false);
 expect(list.querySelectorAll('button')).toHaveLength(150);
 list.scrollTop = 840;
 await click(button('v90'));
 expect(container.querySelector('[aria-label="Version list"]')).toBe(list);
 expect(list.scrollTop).toBe(840);
 expect(button('v90').getAttribute('aria-pressed')).toBe('true');
 expect(button('v90').classList.contains('version-selected')).toBe(true);
 expect(button('v150').getAttribute('aria-pressed')).toBe('false');
 await reply(1, { version: 90, schema: '"selected"' });
 expect(list.scrollTop).toBe(840);
 expect(schema().value).toBe('"selected"');
 expect(schema().closest('.registry-versions')).toBeNull();
});
it('rejects stale fetch/version successes and failures, including edits and tab changes', async () => {
 await registry(); await config(); await submit(); await submit();
 await reply(1, { versions: [3, 1] }); await reply(0, { versions: [99], version: 99, schema: '"old"' });
 expect(button('v99')).toBeUndefined();
 await click(button('v1')); await click(button('v3')); await reply(3, { version: 3, schema: '"current"' });
 await act(async () => requests[2].reject(new Error('stale transport'))); expect(schema().value).toBe('"current"');
 await click(button('v1')); await edit('Topic Name', 'new-topic'); await reply(4, { schema: '"stale edit"' });
 expect(schema().value).toBe(''); expect(container.querySelectorAll('nav button')).toHaveLength(0);
 await submit(); await click(tab('String')); await registry(); await reply(5, { versions: [10] });
 expect(schema().value).toBe(''); expect(button('v10')).toBeUndefined();
});
it('clears stale output for backend/transport/malformed failures and permits retry', async () => {
 await registry(); await config(); await submit(); await reply(0, { versions: [3, 1] });
 await click(button('v1')); await reply(1, { success: false, message: 'Access denied (HTTP 403).', schema: '', version: 0 });
 expect(status()).toContain('403'); expect(schema().value).toBe(''); expect(container.querySelector('[aria-label="Copy Avro Schema"]').disabled).toBe(true);
 await submit(); await act(async () => requests[2].reject(new Error('private details')));
 expect(status()).toContain('Could not contact the Go backend'); expect(status()).not.toContain('private details');
 await submit(); await act(async () => requests[3].resolve(null)); expect(status()).toContain('Go backend');
 await submit(); await reply(4, { versions: [], version: 0, schema: '', message: 'No versions found for this subject.' }); expect(status()).toContain('No versions');
});

const profileA = { id: 'a'.repeat(32), name: 'Development', url: 'https://dev.example.com', keystorePath: '/dev/key.p12', truststorePath: '/dev/trust.jks', hasPassword: true };
const profileB = { id: 'b'.repeat(32), name: 'Production', url: 'https://prod.example.com', keystorePath: '', truststorePath: '', hasPassword: false };
const connectionA = { url: profileA.url, keystorePath: profileA.keystorePath, truststorePath: profileA.truststorePath, password: 'secret-A' };
const connectionB = { url: profileB.url, keystorePath: '', truststorePath: '', password: '' };
const profileResult = (patch = {}) => ({ success: true, warning: false, message: 'Profile applied.', profiles: [profileA, profileB], selectedID: profileA.id, config: connectionA, credentialAvailable: true, ...patch });
async function restoreProfile() {
 ProfileService.List.mockResolvedValue(profileResult({ config: null }));
 ProfileService.Apply.mockImplementation(async (id) => profileResult({ selectedID: id, config: id === profileA.id ? connectionA : id === profileB.id ? connectionB : { ...defaultState } }));
 await registry();
}
async function chooseProfile(id) {
 const disclosure = container.querySelector('[aria-controls="registry-profiles"]'); if (disclosure.getAttribute('aria-expanded') === 'false') await click(disclosure);
 await act(async () => { const e = container.querySelector('select[aria-label="Profile"]'); e.value = id; e.dispatchEvent(new Event('change', { bubbles: true })); });
}
const modalButton = (text) => [...document.querySelectorAll('[role="dialog"] button')].find((e) => e.textContent === text);
async function profileName(value) {
 await act(async () => { const e = document.querySelector('input[aria-label="Profile Name"]'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(e, value); e.dispatchEvent(new Event('input', { bubbles: true })); });
}
it('restores the selected complete profile from Go without localStorage credentials or Topic', async () => {
 await restoreProfile();
 expect(field('Schema Registry URL').value).toBe(connectionA.url); expect(field('Password').value).toBe('secret-A');
 expect(field('Topic Name').value).toBe('');
 expect(JSON.stringify(window.localStorage)).not.toContain('secret-A');
 await edit('Topic Name', 'query-only'); expect(container.textContent).not.toContain('Unsaved changes');
 await edit('Schema Registry URL', 'https://unsaved.example.com'); await click(tab('String')); await registry();
 expect(field('Schema Registry URL').value).toBe('https://unsaved.example.com'); expect(ProfileService.Apply).toHaveBeenCalledTimes(1);
 await act(async () => root.unmount()); await mount();
 expect(field('Password').value).toBe('secret-A'); expect(field('Topic Name').value).toBe('');
});
it('keeps dirty inputs until confirmed Apply and replaces URL, paths and password together while keeping Topic', async () => {
 await restoreProfile(); await edit('Topic Name', 'very-long-topic'); await edit('Schema Registry URL', 'https://edited.example.com');
 await submit(); expect(requests).toHaveLength(1);
 await chooseProfile(profileB.id); expect(field('Schema Registry URL').value).toBe('https://edited.example.com');
 expect(container.textContent).toContain('Not applied'); expect(container.textContent).toContain('Unsaved changes'); expect(button('Fetch').disabled).toBe(true);
 await click(button('Apply')); expect(document.querySelector('[role="dialog"]').textContent).toContain('Unsaved connection');
 await click(modalButton('Cancel')); expect(field('Password').value).toBe('secret-A');
 await click(button('Apply')); await click(modalButton('Discard and apply'));
 expect(field('Schema Registry URL').value).toBe(connectionB.url); expect(field('Keystore Path').value).toBe(''); expect(field('Truststore Path').value).toBe(''); expect(field('Password').value).toBe('');
 expect(field('Topic Name').value).toBe('very-long-topic'); expect(container.textContent).not.toContain('Unsaved changes'); expect(button('Fetch').disabled).toBe(false);
 await reply(0, { schema: '"old environment"', versions: [90] }); expect(schema().value).toBe(''); expect(button('v90')).toBeUndefined();
});
it('Save As excludes Topic, Save updates the applied profile and Delete preserves current inputs in Default Profile', async () => {
 await registry(); await config();
 ProfileService.Save.mockResolvedValue(profileResult({ profiles: [profileB], selectedID: profileB.id, config: null, message: 'Profile saved locally.' }));
 await click(button('Save As')); await profileName('Personal'); await click(modalButton('Save profile'));
 expect(ProfileService.Save).toHaveBeenLastCalledWith('', 'Personal', { url: 'https://registry.example.com', keystorePath: '', truststorePath: '', password: '' });
 expect(container.textContent).not.toContain('Unsaved changes');
 await edit('Schema Registry URL', 'https://changed.example.com');
 await click(button('Save')); expect(ProfileService.Save.mock.calls.at(-1)[0]).toBe(profileB.id); expect(ProfileService.Save.mock.calls.at(-1)[2].topic).toBeUndefined();
 expect(container.textContent).not.toContain('Unsaved changes');
 ProfileService.Delete.mockResolvedValue(profileResult({ profiles: [], selectedID: '', config: null, message: 'Profile deleted.' }));
 await click(button('Delete')); await click(modalButton('Delete profile'));
 expect(ProfileService.Delete).toHaveBeenCalledWith(profileB.id); expect(field('Schema Registry URL').value).toBe('https://registry.example.com');
 expect(container.querySelector('select[aria-label="Profile"]').value).toBe(''); expect(button('Save').disabled).toBe(true);
});
it('reports legacy password re-entry and local file failures without replacing inputs', async () => {
 ProfileService.List.mockResolvedValue(profileResult({ config: null }));
 ProfileService.Apply.mockResolvedValue(profileResult({ config: { ...connectionA, password: '' }, credentialAvailable: false, warning: true, message: 'Legacy password must be re-entered and saved locally.' }));
 await registry(); expect(container.textContent).toContain('Legacy password must be re-entered'); expect(field('Password').value).toBe('');
 expect(container.querySelector('[aria-label="SSL configuration status"]').textContent).toContain('Password');
 await edit('Password', 'replacement');
 ProfileService.Save.mockResolvedValue(profileResult({ success: false, message: 'Could not save local profile file.' }));
 await click(button('Save')); expect(field('Password').value).toBe('replacement'); expect(container.textContent).toContain('Could not save local profile file'); expect(container.textContent).toContain('Unsaved changes');
 expect(JSON.stringify(window.localStorage)).not.toContain('replacement');
});
it('preserves unsaved Registry drafts across tab changes, keeps full long topic payload and exposes tail-preserving subject labels', async () => {
 await registry(); await config();
 const long = 'environment.application.'.repeat(30) + 'member-order-created'; await edit('Topic Name', long);
 await submit(); expect(requests[0].config.topic).toBe(long); await reply(0, { versions: [3] });
 for (const name of ['Versions subject', 'Schema subject']) expect(container.querySelector(`[aria-label="${name}"]`).textContent).toBe(long + '-value');
 await click(tab('String')); await registry(); expect(field('Topic Name').value).toBe(long); expect(field('Schema Registry URL').value).toBe('https://registry.example.com');
 expect(container.querySelector('[aria-label="Profile status"]').textContent).toContain('Saved'); expect(schema().value).toBe('');
});
it('handles corrupt profile metadata and missing saved selection with Default Profile still usable', async () => {
 ProfileService.List.mockResolvedValue(profileResult({ success: false, profiles: [], selectedID: '', config: null, message: 'Invalid profile storage. Use Default Profile.' }));
 await registry(); expect(container.textContent).toContain('Invalid profile storage'); await config(); expect(ProfileService.SaveDefault).not.toHaveBeenCalled(); await submit(); expect(requests).toHaveLength(1);
 await act(async () => root.unmount());
 ProfileService.List.mockResolvedValue(profileResult({ selectedID: 'missing', config: null })); ProfileService.Apply.mockResolvedValue(profileResult({ success: false, message: 'Selected profile no longer exists.' }));
 await mount(); expect(container.textContent).toContain('no longer exists'); expect(field('Schema Registry URL').value).toBe('');
});

it('keeps Profile and SSL as collapsed compact disclosures by default', async () => {
 await registry();
 for (const id of ['registry-profiles', 'registry-ssl']) {
  const toggle = container.querySelector(`[aria-controls="${id}"]`);
  expect(toggle.getAttribute('aria-expanded')).toBe('false'); expect(container.querySelector(`#${id}`).style.display).toBe('none');
  await click(toggle); expect(container.querySelector(`#${id}`).style.display).not.toBe('none');
 }
 expect(container.querySelector('select[aria-label="Profile"]').options[0].textContent).toBe('Default Profile');
 expect(field('Schema Registry URL').closest('.inline-field')).not.toBeNull(); expect(field('Topic Name').closest('.inline-field')).not.toBeNull();
});
it('serializes/coalesces Default autosaves and persists a revert while an older write is pending', async () => {
 const saves = [];
 ProfileService.SaveDefault.mockImplementation((connection, changed) => new Promise((resolve) => saves.push({ connection, changed, resolve })));
 await registry(); await edit('Schema Registry URL', 'https://first.example.com');
 await edit('Schema Registry URL', 'https://second.example.com'); await edit('Schema Registry URL', 'https://latest.example.com');
 expect(saves).toHaveLength(1); expect(field('Schema Registry URL').disabled).toBe(false);
 await act(async () => saves[0].resolve(profileResult({ profiles: [], selectedID: '', config: null, message: 'Default saved.' })));
 expect(saves).toHaveLength(2); expect(saves[1].connection.url).toBe('https://latest.example.com');
 await edit('Schema Registry URL', 'https://first.example.com');
 await act(async () => saves[1].resolve(profileResult({ profiles: [], selectedID: '', config: null, message: 'Default saved.' })));
 expect(saves).toHaveLength(3); expect(saves[2].connection.url).toBe('https://first.example.com');
 await act(async () => saves[2].resolve(profileResult({ profiles: [], selectedID: '', config: null, message: 'Default saved.' })));
 expect(container.querySelector('[aria-label="Profile status"]').textContent).toContain('Saved');
 expect(saves.every((save) => !save.changed && !('topic' in save.connection))).toBe(true);
});
it('never autosaves named edits into Default and only marks explicit password edits for local writes', async () => {
 await restoreProfile(); await edit('Schema Registry URL', 'https://named-edit.example.com'); await edit('Password', 'named-change');
 expect(ProfileService.SaveDefault).not.toHaveBeenCalled();
 await chooseProfile(''); await click(button('Apply')); await click(modalButton('Discard and apply'));
 expect(ProfileService.SaveDefault).not.toHaveBeenCalled();
 await edit('Schema Registry URL', 'htt'); expect(ProfileService.SaveDefault.mock.calls.at(-1)[1]).toBe(false);
 await edit('Password', 'default-password'); expect(ProfileService.SaveDefault.mock.calls.at(-1)[1]).toBe(true);
 await edit('Truststore Path', '/partial/trust.jks'); expect(ProfileService.SaveDefault.mock.calls.at(-1)[1]).toBe(false);
 expect(JSON.stringify(window.localStorage)).not.toContain('default-password');
});
it('shows local autosave failure and allows an explicit retry', async () => {
 await registry(); ProfileService.SaveDefault.mockResolvedValueOnce(profileResult({ success: false, profiles: [], selectedID: '', config: null, message: 'Local profile file unavailable.' }));
 await edit('Password', 'default-secret');
 expect(container.querySelector('[aria-label="Profile status"]').textContent).toContain('Attention');
 expect(field('Password').value).toBe('default-secret');
 await click(button('Save')); expect(ProfileService.SaveDefault).toHaveBeenCalledTimes(2);
 expect(container.querySelector('[aria-label="Profile status"]').textContent).toContain('Saved');
 expect(JSON.stringify(window.localStorage)).not.toContain('default-secret');
});
