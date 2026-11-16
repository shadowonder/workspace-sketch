import { useCallback, useEffect, useRef, useState } from 'react';
import { Badge, Button, Collapse, Group, Loader, Paper, PasswordInput, Text, TextInput } from '@mantine/core';
import { RegistryService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import Editor from './Editor.jsx';
import RegistryProfiles from './RegistryProfiles.jsx';
import Icon from './RegistryIcon.jsx';

// Request state is transient; ProfileService stores only connection metadata
// and OS-vault credentials. Topic always remains query-specific.
const emptyConfig = { url: '', topic: '', keystorePath: '', truststorePath: '', password: '' };
const sslFields = [['keystorePath', 'Keystore Path'], ['truststorePath', 'Truststore Path'], ['password', 'Password']];
function sslStatus(config) {
  if (sslFields.every(([key]) => !config[key])) return { text: 'Not configured (optional)', missing: [] };
  const missing = sslFields.filter(([key]) => key === 'password' ? !config[key] : !config[key].trim()).map(([, label]) => label);
  return { text: missing.length ? `Missing: ${missing.join(', ')}` : 'Configured (not verified)', missing };
}
function validResponse(response) {
  if (!response || typeof response.success !== 'boolean' || typeof response.message !== 'string' ||
      typeof response.schema !== 'string' || !Array.isArray(response.versions) ||
      !response.versions.every((v) => Number.isInteger(v) && v > 0) || !Number.isInteger(response.version)) {
    throw new Error('Invalid bridge response');
  }
  return response;
}

function SubjectTail({ subject, label }) {
  return <div className="subject-tail" title={subject} aria-label={label}><bdi dir="ltr">{subject || 'No subject selected'}</bdi></div>;
}

export default function SchemaRegistry({ active = true }) {
  const [config, setConfig] = useState(emptyConfig);
  const [sslOpen, setSslOpen] = useState(false);
  const [versions, setVersions] = useState([]);
  const [selected, setSelected] = useState(null);
  const [schema, setSchema] = useState('');
  const [state, setState] = useState({ status: 'idle', message: 'Enter a registry URL and topic, then Fetch.' });
  const requestId = useRef(0);
  const topicInput = useRef(null);
  const [profileBusy, setProfileBusy] = useState(true);
  const [passwordRevision, setPasswordRevision] = useState(0);
  const [profilePending, setProfilePending] = useState(false);
  const invalidate = useCallback(() => {
    requestId.current += 1; setVersions([]); setSelected(null); setSchema('');
    setState({ status: 'idle', message: 'Connection changed — click Fetch.' });
  }, []);
  const replaceConnection = useCallback((connection) => {
    invalidate(); setConfig((previous) => ({ ...connection, topic: previous.topic }));
  }, [invalidate]);
  useEffect(() => { invalidate(); }, [active, invalidate]);
  useEffect(() => {
    if (topicInput.current && document.activeElement !== topicInput.current) topicInput.current.scrollLeft = topicInput.current.scrollWidth;
  }, [config.topic]);
  useEffect(() => () => { requestId.current += 1; }, []);
  const ssl = sslStatus(config);

  function edit(key, value) {
    if (key === 'password') setPasswordRevision((revision) => revision + 1);
    requestId.current += 1;
    setConfig((previous) => ({ ...previous, [key]: value }));
    setVersions([]); setSelected(null); setSchema('');
    setState({ status: 'idle', message: 'Configuration changed — click Fetch.' });
  }
  async function load(version = null) {
    if (profileBusy || !active) return;
    if (profilePending) { setState({ status: 'error', message: 'Apply the selected profile or select the current connection before Fetch.' }); return; }
    const id = ++requestId.current;
    setSchema('');
    setSelected(version);
    if (version === null) setVersions([]);
    const missing = !config.url.trim() ? 'Schema Registry URL is required.'
      : !config.topic.trim() ? 'Topic Name is required.'
        : ssl.missing.length ? `SSL configuration missing: ${ssl.missing.join(', ')}.` : '';
    if (missing) { setState({ status: 'error', message: missing }); return; }
    setState({ status: 'loading', message: version === null ? 'Fetching versions…' : `Loading version ${version}…` });
    try {
      const response = validResponse(await (version === null ? RegistryService.Fetch(config) : RegistryService.Schema(config, version)));
      if (id !== requestId.current) return;
      if (version === null) setVersions(response.versions);
      if (!response.success) {
        setState({ status: 'error', message: response.message });
        return;
      }
      setSelected(response.version || null); setSchema(response.schema);
      setState({ status: 'done', message: response.message });
    } catch {
      if (id !== requestId.current) return;
      setState({ status: 'error', message: 'Could not contact the Go backend. Please try again.' });
    }
  }
  const loading = state.status === 'loading';
  const subject = config.topic ? `${config.topic}-value` : '';
  const sslValid = ssl.missing.length === 0;
  return <div className="registry-tool">
    <div className="registry-controls">
      <div className="registry-connection">

        <form onSubmit={(event) => { event.preventDefault(); void load(); }} noValidate>
          <Group gap="sm" align="end" wrap="nowrap" className="registry-url-row">
            <TextInput size="xs" className="registry-url inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Schema Registry URL" aria-label="Schema Registry URL" required
              leftSection={<Icon name="connection" />} disabled={profileBusy}
              placeholder="https://registry.example.com" value={config.url} onChange={(e) => edit('url', e.target.value)} autoComplete="off" />
            <Button size="xs" type="submit" leftSection={<Icon name="fetch" />} disabled={profileBusy || profilePending}>Fetch</Button>
          </Group>
          <TextInput size="xs" className="registry-topic inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Topic Name" aria-label="Topic Name" required ref={topicInput}
            disabled={profileBusy} placeholder="namespace.member-order-created" value={config.topic}
            onChange={(e) => edit('topic', e.target.value)} autoComplete="off"
            onBlur={(e) => { e.currentTarget.scrollLeft = e.currentTarget.scrollWidth; }} />
        </form>
      </div>
      <RegistryProfiles config={config} passwordRevision={passwordRevision} onReplace={replaceConnection} onInvalidate={invalidate} onBusy={setProfileBusy} onPending={setProfilePending} />
      <section className="registry-disclosure ssl-section">
        <div className="registry-divider">
          <Button size="compact-xs" variant="transparent" color={sslValid ? 'green' : 'red'} leftSection={<Icon name="shield" />}
            aria-expanded={sslOpen} aria-controls="registry-ssl" onClick={() => setSslOpen((open) => !open)}>
            {sslOpen ? '▾' : '▸'} SSL configuration
          </Button>
          <Badge size="xs" variant="light" color={sslValid ? 'green' : 'red'} leftSection={<Icon name={sslValid ? 'check' : 'alert'} size={12} />}
            aria-label="SSL configuration status">{ssl.text}</Badge>
        </div>
        <Collapse expanded={sslOpen} id="registry-ssl" keepMountedMode="display-none" transitionDuration={0}>
          <div className="registry-ssl-fields">
            {sslFields.slice(0, 2).map(([key, label]) => <TextInput key={key} size="xs" className="inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label={label} aria-label={label} value={config[key]}
              disabled={profileBusy} autoComplete="off" placeholder={key === 'keystorePath' ? '/path/client.p12' : '/path/truststore.jks'} onChange={(e) => edit(key, e.target.value)} />)}
            <PasswordInput size="xs" className="inline-field" classNames={{ label: 'inline-label', wrapper: 'inline-control' }} label="Password" aria-label="Password" autoComplete="new-password" value={config.password} disabled={profileBusy} onChange={(e) => edit('password', e.target.value)} />
          </div>
        </Collapse>
      </section>
    </div>
    <Group role="status" aria-live="polite" className="registry-status" gap="xs" c={state.status === 'error' ? 'red' : state.status === 'done' ? 'green' : 'dimmed'}>
      {loading ? <Loader size={12} /> : <Icon name={state.status === 'error' ? 'alert' : state.status === 'done' ? 'check' : 'connection'} size={14} />}<Text size="xs">{state.message}</Text>
    </Group>
    <div className="registry-results" aria-busy={loading}>
      <Paper withBorder className="registry-versions" radius="md" component="nav" aria-label="Schema versions">
        <div className="versions-heading"><Text size="xs" fw={600}>Versions</Text><Badge size="xs" variant="light" color="gray">{versions.length}</Badge></div>
        <SubjectTail subject={subject} label="Versions subject" />
        <div className="version-list" aria-label="Version list">
          {versions.map((version) => <Button key={version} variant={selected === version ? 'light' : 'subtle'} size="xs" fullWidth
            className={selected === version ? 'version-selected' : ''} leftSection={<Icon name="file" size={13} />}
            disabled={profileBusy} aria-pressed={selected === version} onClick={() => void load(version)}>v{version}</Button>)}
          {!versions.length && <div className="versions-empty"><Icon name="file" size={22} /><Text size="xs" c="dimmed">{loading ? 'Loading…' : 'No versions yet'}</Text></div>}
        </div>
      </Paper>
      <Editor name="Avro Schema" readOnly value={schema} canCopy={state.status === 'done' && !!schema} busy={loading}
        placeholder="Fetch a topic to inspect its Avro schema."
        headerDetail={<Group gap={6} wrap="nowrap"><SubjectTail subject={subject} label="Schema subject" />{selected && <Badge size="xs" variant="light">v{selected}</Badge>}</Group>} />
    </div>
  </div>;
}
