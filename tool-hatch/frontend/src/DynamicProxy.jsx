import { useCallback, useEffect, useRef, useState } from 'react';
import { Badge, Button, Collapse, Group, Loader, Modal, NativeSelect, Paper, Switch, Text, TextInput } from '@mantine/core';
import { ProxyService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import Editor from './Editor.jsx';
import Icon from './RegistryIcon.jsx';
import ProxyConsole from './ProxyConsole.jsx';
import ProxyInspector from './ProxyInspector.jsx';

// Go owns the server, forwarding, streaming, script execution, Debug captures
// and persistence. React keeps the editable draft, mirrors the polled Go status
// and only sees metadata-only activity plus previews fetched on demand.
export const defaultScript = `// Proxy Script: define onRequest, onResponse, both, or neither (plain forwarding). Cmd/Ctrl+F finds text.
// onRequest(req) runs before the request is forwarded:
//   req.method, req.path (writable), req.body (string; read/write, max 10 MiB)
//   req.header(name), req.headerValues(name), req.headers(), req.setHeader/addHeader(name, value), req.removeHeader(name)
//   req.query(name), req.setQuery(name, value), req.removeQuery(name)
// onResponse(res) runs when the upstream response headers arrive; streams (SSE) are never delayed:
//   res.status (writable, 200-599), res.request.method/path, same header functions as req
//   res.bodyAvailable; res.body (string; read/write) only for known-length, uncompressed text/JSON up to 1 MiB.
//   SSE, NDJSON, chunked, compressed and binary bodies pass through untouched (reading res.body throws).
// In both hooks:
//   http.request({ url, method, headers, body, timeoutMs }) -> { status, statusText, headers, body, header(name), json() }
//   console.log/info/warn/error/debug(...) -> Console panel (memory only; avoid printing secrets)
//   context: string cache shared by all requests and both hooks of this running proxy (emptied on Stop/Start)
//     context.get(key) -> string or null; context.set(key, value, ttlSeconds?); context.delete(key)
//     context.getOrLoad(key, () => ({ value, ttlSeconds })) -> runs the loader once per key, even concurrently
// Throwing answers HTTP 500 instead of forwarding (onRequest) or instead of the upstream response (onResponse).
function onRequest(req) {
  // Example: obtain a token once, reuse it until shortly before it expires, and attach it.
  // const token = context.getOrLoad('token', () => {
  //   const started = Date.now();
  //   const r = http.request({ method: 'POST', url: 'https://sso.example.com/token', body: { client: 'cli' } });
  //   if (r.status !== 200) throw new Error('token request failed: ' + r.status); // failures are not cached
  //   const data = r.json(), elapsed = (Date.now() - started) / 1000;
  //   return { value: data.access_token, ttlSeconds: Math.max(1, data.expires_in - elapsed - 60) };
  // });
  // req.setHeader('Authorization', 'Bearer ' + token);
}

// function onResponse(res) {
//   // Example: forget a token the upstream rejected; this 401 is still returned (no automatic retry).
//   if (res.status === 401) context.delete('token');
// }
`;
const defaults = { listenPort: '8080', targetURL: '', sourcePath: '', targetPath: '', script: defaultScript };
const pollInterval = 1000;

function fromGo(config) {
  if (!config || (!config.listenPort && !config.targetURL && !config.sourcePath && !config.targetPath && !config.script)) return { ...defaults };
  return { listenPort: config.listenPort ? String(config.listenPort) : '', targetURL: config.targetURL ?? '', sourcePath: config.sourcePath ?? '', targetPath: config.targetPath ?? '', script: config.script ?? '' };
}
function toGo(config) {
  const port = /^\d{1,5}$/.test(config.listenPort.trim()) ? Number(config.listenPort.trim()) : 0;
  return { listenPort: port, targetURL: config.targetURL, sourcePath: config.sourcePath, targetPath: config.targetPath, script: config.script };
}
const same = (a, b) => JSON.stringify(toGo(a)) === JSON.stringify(toGo(b));
function validStatus(status) {
  if (!status || typeof status.running !== 'boolean' || typeof status.message !== 'string' || !Array.isArray(status.activity)) throw new Error('Invalid bridge response');
  return status;
}
function validStore(response) {
  if (!response || typeof response.success !== 'boolean' || !Array.isArray(response.saved)) throw new Error('Invalid bridge response');
  return response;
}
const activityLimits = ['100', '200', '500'];
function statusColor(entry) {
  if (entry.pending) return 'blue';
  if (entry.error || entry.status >= 500) return 'red';
  if (entry.status >= 400) return 'yellow';
  return 'green';
}

export default function DynamicProxy({ active = true }) {
  const [config, setConfig] = useState(defaults);
  // Autosave starts only after the stored draft was restored, so a failed read
  // can never overwrite an unseen draft with defaults.
  const [loaded, setLoaded] = useState(false);
  const [mappingOpen, setMappingOpen] = useState(false);
  const [savedOpen, setSavedOpen] = useState(false);
  const [server, setServer] = useState({ running: false, address: '', target: '', requests: 0, failures: 0, lastError: '', activity: [], consoleLastID: 0 });
  const [action, setAction] = useState({ busy: false, message: 'Stopped.', error: false });
  const [startedWith, setStartedWith] = useState(null);
  const [saved, setSaved] = useState([]);
  const [selectedName, setSelectedName] = useState('');
  const [saveName, setSaveName] = useState('');
  const [store, setStore] = useState({ state: 'loading', message: 'Loading saved configuration…' });
  const [confirmLoad, setConfirmLoad] = useState(null);
  const [selectedId, setSelectedId] = useState(0);
  const [inspectorOpen, setInspectorOpen] = useState(false);
  const [captureRefresh, setCaptureRefresh] = useState(0);
  const actionEpoch = useRef(0);
  const acting = useRef(0); // number of status-returning actions in flight
  const draftQueue = useRef({ running: false, pending: null });
  const shownStatus = useRef('');
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  // Start/Stop responses are authoritative. A poll is discarded if any action
  // was issued or completed while it was in flight (e.g. a poll racing Stop).
  // Identical polls (the idle case) do not re-render the tab.
  const showStatus = useCallback((status) => {
    if (!status.running) setStartedWith(null);
    const key = JSON.stringify(status);
    if (key === shownStatus.current) return;
    shownStatus.current = key;
    setServer(status);
  }, []);
  const poll = useCallback(async () => {
    const issued = actionEpoch.current;
    const status = validStatus(await ProxyService.Status());
    if (mounted.current && acting.current === 0 && issued === actionEpoch.current) {
      showStatus(status);
      // Small stream chunks reuse allocated capture memory, so debugBytes and
      // Activity can stay identical while the selected preview keeps growing.
      if (status.activity.some((entry) => entry.debug && entry.pending)) setCaptureRefresh((n) => n + 1);
    }
  }, [showStatus]);

  useEffect(() => {
    let cancelled = false;
    ProxyService.ListConfigs().then(validStore).then((response) => {
      if (cancelled) return;
      if (response.success) {
        setConfig(fromGo(response.draft)); setSaved(response.saved);
        setStore({ state: 'saved', message: 'Draft restored.' }); setLoaded(true);
      } else {
        setStore({ state: 'error', message: response.message });
      }
    }).catch(() => { if (!cancelled) setStore({ state: 'error', message: 'Could not load saved proxy configurations.' }); });
    return () => { cancelled = true; };
  }, []);

  // Poll the actual Go server state while the tab is visible. The server itself
  // keeps running regardless of the selected tab.
  useEffect(() => {
    if (!active) return undefined;
    const tick = () => { poll().catch(() => {}); };
    tick();
    const timer = setInterval(tick, pollInterval);
    return () => clearInterval(timer);
  }, [active, poll]);

  // Serialized draft autosave: one write at a time, latest snapshot wins.
  const saveDraft = useCallback(async (snapshot) => {
    const queue = draftQueue.current;
    queue.pending = snapshot;
    if (queue.running) return;
    queue.running = true;
    while (queue.pending) {
      const next = queue.pending; queue.pending = null;
      if (mounted.current) setStore({ state: 'saving', message: 'Saving draft…' });
      try {
        const response = validStore(await ProxyService.SaveDraft(toGo(next)));
        if (mounted.current) setStore(response.success ? { state: 'saved', message: 'Draft saved.' } : { state: 'error', message: response.message });
      } catch {
        if (mounted.current) setStore({ state: 'error', message: 'Could not save the draft.' });
      }
    }
    queue.running = false;
  }, []);

  function edit(key, value) {
    const next = { ...config, [key]: value };
    setConfig(next);
    if (loaded) void saveDraft(next);
  }

  async function startStop() {
    setAction({ busy: true, message: server.running ? 'Stopping…' : 'Validating and starting…', error: false });
    const snapshot = config;
    acting.current += 1; actionEpoch.current += 1;
    try {
      const status = validStatus(await (server.running ? ProxyService.Stop() : ProxyService.Start(toGo(snapshot))));
      acting.current -= 1; actionEpoch.current += 1;
      if (!mounted.current) return;
      showStatus(status);
      if (status.running && status.success) setStartedWith(snapshot);
      setAction({ busy: false, message: status.message || (status.running ? 'Proxy running.' : 'Stopped.'), error: !status.success });
    } catch {
      acting.current -= 1; actionEpoch.current += 1;
      if (mounted.current) setAction({ busy: false, message: 'Could not contact the Go backend. Please try again.', error: true });
    }
  }

  // Debug switch, Activity retention and Clear return the authoritative status,
  // guarded against overlapping polls like Start/Stop.
  async function statusAction(call) {
    acting.current += 1; actionEpoch.current += 1;
    try {
      const status = validStatus(await call());
      acting.current -= 1; actionEpoch.current += 1;
      if (!mounted.current) return;
      showStatus(status);
      if (!status.success) setAction({ busy: false, message: status.message, error: true });
    } catch {
      acting.current -= 1; actionEpoch.current += 1;
      if (mounted.current) setAction({ busy: false, message: 'Could not contact the Go backend. Please try again.', error: true });
    }
  }
  function select(entry) {
    if (!entry.debug) return;
    setSelectedId(entry.id); setInspectorOpen(true);
  }

  async function storeCall(call, after) {
    setStore({ state: 'saving', message: 'Working…' });
    try {
      const response = validStore(await call());
      if (!mounted.current) return;
      if (!response.success) { setStore({ state: 'error', message: response.message }); return; }
      setSaved(response.saved); after?.(response);
      setStore({ state: 'saved', message: response.message });
    } catch {
      if (mounted.current) setStore({ state: 'error', message: 'Could not update saved configurations.' });
    }
  }
  function load(name) {
    const entry = saved.find((item) => item.name === name);
    if (!entry) return;
    const next = fromGo(entry.config);
    setConfirmLoad(null); setConfig(next); setSaveName(entry.name);
    if (next.sourcePath || next.targetPath) setMappingOpen(true);
    if (loaded) void saveDraft(next);
  }
  function requestLoad() {
    const entry = saved.find((item) => item.name === selectedName);
    if (!entry) return;
    const unsaved = !saved.some((item) => same(fromGo(item.config), config));
    if (unsaved) setConfirmLoad(entry.name); else load(entry.name);
  }

  const restartNeeded = server.running && startedWith && !same(startedWith, config);
  const mappingActive = config.sourcePath.trim() !== '' || config.targetPath.trim() !== '';
  const busyStore = store.state === 'loading';
  const activity = [...server.activity].reverse();
  const selected = selectedId ? server.activity.find((entry) => entry.id === selectedId) ?? null : null;
  return <div className="proxy-tool">
    <div className="registry-controls proxy-controls">
      <form onSubmit={(event) => { event.preventDefault(); void startStop(); }} noValidate>
        <Group gap="sm" align="end" wrap="nowrap" className="proxy-connection-row">
          <TextInput size="xs" className="proxy-port inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Local Port" aria-label="Local Port"
            leftSection={<Text size="xs" c="dimmed">127.0.0.1:</Text>} leftSectionWidth={64} inputMode="numeric" autoComplete="off"
            value={config.listenPort} disabled={busyStore} onChange={(e) => edit('listenPort', e.target.value)} />
          <TextInput size="xs" className="proxy-target inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Target" aria-label="Target URL"
            leftSection={<Icon name="connection" />} placeholder="https://api.example.com:443" autoComplete="off" spellCheck={false}
            value={config.targetURL} disabled={busyStore} onChange={(e) => edit('targetURL', e.target.value)} />
          <Switch size="xs" label="Debug" className="proxy-debug" checked={!!server.debug} aria-label="Debug capture"
            title="Capture request/response headers and bodies of new requests in memory for the Inspector"
            onChange={(e) => void statusAction(() => ProxyService.SetDebug(e.currentTarget.checked))} />
          <Button size="xs" type="submit" className="proxy-start" color={server.running ? 'red' : 'blue'} disabled={action.busy || busyStore}
            leftSection={action.busy ? <Loader size={12} color="white" /> : <Icon name={server.running ? 'stop' : 'play'} />}>
            {server.running ? 'Stop' : 'Start'}
          </Button>
        </Group>
      </form>
      <section className="registry-disclosure">
        <div className="registry-divider">
          <Button size="compact-xs" variant="transparent" color="gray" leftSection={<Icon name="chevron" />}
            aria-expanded={mappingOpen} aria-controls="proxy-mapping" onClick={() => setMappingOpen((open) => !open)}>
            {mappingOpen ? '▾' : '▸'} Path Mapping
          </Button>
          <Badge size="xs" variant="light" color={mappingActive ? 'blue' : 'gray'} aria-label="Path mapping status">
            {mappingActive ? `${config.sourcePath || '?'} → ${config.targetPath || '?'}` : 'Off — paths forwarded unchanged'}
          </Badge>
        </div>
        <Collapse expanded={mappingOpen} id="proxy-mapping" keepMountedMode="display-none" transitionDuration={0}>
          <div className="proxy-mapping-fields">
            <TextInput size="xs" className="inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Source Path" aria-label="Source Path"
              placeholder="/api" autoComplete="off" spellCheck={false} value={config.sourcePath} disabled={busyStore} onChange={(e) => edit('sourcePath', e.target.value)} />
            <TextInput size="xs" className="inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Target Path" aria-label="Target Path"
              placeholder="/v1" autoComplete="off" spellCheck={false} value={config.targetPath} disabled={busyStore} onChange={(e) => edit('targetPath', e.target.value)} />
          </div>
        </Collapse>
      </section>
      <section className="registry-disclosure">
        <div className="registry-divider">
          <Button size="compact-xs" variant="transparent" color={store.state === 'error' ? 'red' : 'gray'} leftSection={<Icon name="save" />}
            aria-expanded={savedOpen} aria-controls="proxy-saved" onClick={() => setSavedOpen((open) => !open)}>
            {savedOpen ? '▾' : '▸'} Saved Configurations
          </Button>
          <Badge size="xs" variant="light" color={store.state === 'error' ? 'red' : store.state === 'saving' ? 'yellow' : 'gray'} aria-label="Proxy storage status">{store.message}</Badge>
        </div>
        <Collapse expanded={savedOpen} id="proxy-saved" keepMountedMode="display-none" transitionDuration={0}>
          <Group gap="xs" wrap="nowrap" className="proxy-saved-row">
            <NativeSelect size="xs" aria-label="Saved configuration" className="proxy-saved-select" value={selectedName} disabled={busyStore}
              data={[{ value: '', label: saved.length ? 'Select a configuration…' : 'No saved configurations' }, ...saved.map((item) => ({ value: item.name, label: item.name }))]}
              onChange={(e) => setSelectedName(e.target.value)} />
            <Button size="compact-xs" variant="light" disabled={!selectedName || busyStore} onClick={requestLoad}>Load</Button>
            <Button size="compact-xs" variant="subtle" color="red" leftSection={<Icon name="trash" size={13} />} disabled={!selectedName || busyStore}
              onClick={() => void storeCall(() => ProxyService.DeleteConfig(selectedName), () => setSelectedName(''))}>Delete</Button>
            <TextInput size="xs" aria-label="Configuration Name" placeholder="Configuration name" className="proxy-save-name" value={saveName} disabled={busyStore}
              onChange={(e) => setSaveName(e.target.value)} />
            <Button size="compact-xs" leftSection={<Icon name="save" size={13} />} disabled={!saveName.trim() || busyStore}
              onClick={() => void storeCall(() => ProxyService.SaveConfig(saveName, toGo(config)), (response) => setSelectedName(response.saved.find((item) => item.name.toLowerCase() === saveName.trim().toLowerCase())?.name ?? ''))}>Save</Button>
          </Group>
          <Text size="xs" c="dimmed" className="proxy-storage-note">Stored with the draft in the private local ToolHatch config file; scripts may contain secrets.</Text>
        </Collapse>
      </section>
    </div>
    <Group role="status" aria-live="polite" className="registry-status proxy-status" gap="xs" wrap="nowrap">
      <Badge size="xs" variant="dot" color={server.running ? 'green' : 'gray'} aria-label="Proxy server state">{server.running ? 'Running' : 'Stopped'}</Badge>
      <Text size="xs" c={action.error ? 'red' : 'dimmed'} className="proxy-status-text">
        {server.running ? `${server.address} → ${server.target}` : action.error ? action.message : server.lastError || action.message}
        {server.running && action.error ? ` — ${action.message}` : ''}
      </Text>
      {restartNeeded && <Badge size="xs" variant="light" color="yellow">Changes apply after restart</Badge>}
    </Group>
    <div className="proxy-results">
      <Editor name="Proxy Script" searchable value={config.script} onChange={(e) => edit('script', e.target.value)}
        placeholder="Empty script: requests are forwarded unchanged." />
      <div className="proxy-side">
      <Paper withBorder radius="md" className="proxy-activity" aria-label="Request activity">
        <div className="versions-heading">
          <Text size="xs" fw={600}>Activity</Text>
          <Group gap={4} wrap="nowrap">
            <Badge size="xs" variant="light" color="gray" aria-label="Request count">{server.requests} req</Badge>
            <Badge size="xs" variant="light" color={server.failures ? 'red' : 'gray'} aria-label="Failure count">{server.failures} failed</Badge>
            <NativeSelect size="xs" className="activity-limit" aria-label="Activity retention" title="Entries kept (with their Debug captures)"
              value={String(server.activityLimit || 200)} data={activityLimits}
              onChange={(e) => void statusAction(() => ProxyService.SetActivityLimit(Number(e.currentTarget.value)))} />
            <Button size="compact-xs" variant="subtle" color="gray" aria-label="Clear activity" title="Clear Activity and release Debug captures"
              disabled={!server.activity.length} onClick={() => void statusAction(() => ProxyService.ClearActivity())}><Icon name="trash" size={12} /></Button>
          </Group>
        </div>
        <div className="activity-list">
          {activity.map((entry) => <div key={entry.id} className="activity-entry" data-selected={entry.id === selectedId}
            {...(entry.debug ? { role: 'button', tabIndex: 0, 'aria-pressed': entry.id === selectedId, title: 'Inspect captured exchange',
              onClick: () => select(entry), onKeyDown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); select(entry); } } } : {})}>
            <Group gap={6} wrap="nowrap">
              <Badge size="xs" variant="light" color={statusColor(entry)} className="activity-code">{entry.pending ? '…' : entry.status || 'ERR'}</Badge>
              <Text size="xs" fw={600} className="activity-method">{entry.method}</Text>
              <Text size="xs" className="activity-path" title={entry.path}>{entry.path}</Text>
              {entry.debug && <Badge size="xs" variant="outline" color="violet" className="activity-debug">DEBUG</Badge>}
              <Text size="xs" c="dimmed" className="activity-time">{entry.pending ? 'open' : `${entry.durationMs} ms`}</Text>
            </Group>
            {entry.error && <Text size="xs" c="red" className="activity-error">{entry.error}</Text>}
          </div>)}
          {!activity.length && <div className="versions-empty"><Icon name="connection" size={22} /><Text size="xs" c="dimmed">{server.running ? 'Waiting for requests…' : 'Start the proxy to see requests.'}</Text></div>}
        </div>
      </Paper>
      <ProxyInspector entry={selected} selectedId={selectedId} open={inspectorOpen} onToggle={() => setInspectorOpen((open) => !open)}
        refresh={selected?.pending ? captureRefresh : 0}
        debugBytes={server.debugBytes ?? 0} debugLimit={server.debugLimitBytes ?? 0} />
      <ProxyConsole lastID={server.consoleLastID ?? 0} limit={server.consoleLimit || 500} />
      </div>
    </div>
    <Modal opened={confirmLoad !== null} onClose={() => setConfirmLoad(null)} title="Replace unsaved draft?" size="sm" centered>
      <Text size="sm">Loading “{confirmLoad}” replaces the current draft, which is not saved under a name.</Text>
      <Group justify="flex-end" mt="md" gap="xs">
        <Button size="xs" variant="default" onClick={() => setConfirmLoad(null)}>Cancel</Button>
        <Button size="xs" color="red" onClick={() => load(confirmLoad)}>Replace draft</Button>
      </Group>
    </Modal>
  </div>;
}
