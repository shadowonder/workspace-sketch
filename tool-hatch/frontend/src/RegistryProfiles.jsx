import { useEffect, useRef, useState } from 'react';
import { Badge, Button, Collapse, Group, Modal, NativeSelect, Text, TextInput } from '@mantine/core';
import { ProfileService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import Icon from './RegistryIcon.jsx';

export const defaultConnection = { url: '', keystorePath: '', truststorePath: '', password: '' };
export function connectionOf(config) {
  return Object.fromEntries(Object.keys(defaultConnection).map((key) => [key, config[key] || '']));
}
function responseOf(value) {
  if (!value || typeof value.success !== 'boolean' || !Array.isArray(value.profiles) || typeof value.selectedID !== 'string' || typeof value.message !== 'string') throw new Error('Invalid profile response');
  if (!value.profiles.every((p) => typeof p.id === 'string' && p.id && typeof p.name === 'string')) throw new Error('Invalid profile list');
  return value;
}
function configOf(value) {
  if (!value || Object.keys(defaultConnection).some((key) => typeof value[key] !== 'string')) throw new Error('Invalid profile connection');
  return connectionOf(value);
}

export default function RegistryProfiles({ config, onReplace, onInvalidate, onBusy, onPending, passwordRevision }) {
  const [profiles, setProfiles] = useState([]);
  const [candidate, setCandidate] = useState('');
  const [applied, setApplied] = useState('');
  const [baseline, setBaseline] = useState(defaultConnection);
  const [busy, setBusy] = useState(true);
  const [notice, setNotice] = useState({ message: '', error: false });
  const [dialog, setDialog] = useState(null);
  const operation = useRef(0);
  const [open, setOpen] = useState(false);
  const [restored, setRestored] = useState(false);
  const [autoSaving, setAutoSaving] = useState(false);
  const savedPasswordRevision = useRef(0);
  const revision = useRef(passwordRevision);
  revision.current = passwordRevision;
  const baselineRef = useRef(defaultConnection);
  const queued = useRef(null);
  const running = useRef(false);
  const lastQueued = useRef('');
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  function remember(connection) {
    baselineRef.current = connection; setBaseline(connection);
    savedPasswordRevision.current = revision.current; lastQueued.current = JSON.stringify(connection);
  }

  // Parent callbacks are stable. Guards cover initialization/unmount and restore.
  useEffect(() => {
    const id = ++operation.current;
    async function restore() {
      try {
        const listed = responseOf(await ProfileService.List());
        if (id !== operation.current) return;
        if (!listed.success) { setNotice({ message: listed.message, error: true }); return; }
        setProfiles(listed.profiles);
        {
          const restored = responseOf(await ProfileService.Apply(listed.selectedID));
          if (id !== operation.current) return;
          if (!restored.success) { setNotice({ message: restored.message, error: true }); return; }
          const next = configOf(restored.config);
          setCandidate(restored.selectedID); setApplied(restored.selectedID); remember(next); onReplace(next);
          setNotice({ message: restored.message, error: !!restored.warning }); setRestored(true);
        }
      } catch { if (id === operation.current) setNotice({ message: 'Could not load profiles. Default Profile is available; check profile storage and the Go backend.', error: true }); }
      finally { if (id === operation.current) { setBusy(false); onBusy(false); } }
    }
    void restore();
    return () => { operation.current += 1; };
  }, [onReplace, onBusy]);
  async function persistDefault(connection, passwordVersion) {
    queued.current = { connection, passwordVersion };
    if (running.current) return;
    running.current = true; setAutoSaving(true);
    while (queued.current && alive.current) {
      const job = queued.current; queued.current = null;
      try {
        const result = responseOf(await ProfileService.SaveDefault(job.connection, job.passwordVersion !== savedPasswordRevision.current));
        if (!alive.current) break;
        if (result.success) {
          baselineRef.current = job.connection; setBaseline(job.connection);
          savedPasswordRevision.current = job.passwordVersion;
        }
        setNotice({ message: result.message, error: !result.success || !!result.warning });
      } catch {
        if (alive.current) setNotice({ message: 'Default Profile could not be saved. Check local profile file access and retry.', error: true });
      }
    }
    running.current = false;
    if (alive.current) setAutoSaving(false);
  }
  useEffect(() => {
    if (!restored || busy || applied !== '') return;
    const connection = connectionOf(config), signature = JSON.stringify(connection);
    if (signature === lastQueued.current) return;
    if (!running.current && signature === JSON.stringify(baselineRef.current)) { lastQueued.current = signature; return; }
    lastQueued.current = signature;
    void persistDefault(connection, passwordRevision);
  }, [config, passwordRevision, applied, busy, restored]);
  const dirty = JSON.stringify(connectionOf(config)) !== JSON.stringify(baseline);
  const pending = candidate !== applied;
  useEffect(() => { onPending(pending); }, [pending, onPending]);
  const activeProfile = profiles.find((p) => p.id === applied);

  async function run(kind, target = candidate, name = '') {
    if (busy || running.current) return;
    const id = ++operation.current;
    setBusy(true); onBusy(true); onInvalidate(); setNotice({ message: '', error: false });
    try {
      const result = responseOf(await (kind === 'apply' ? ProfileService.Apply(target)
        : kind === 'delete' ? ProfileService.Delete(target) : ProfileService.Save(kind === 'saveAs' ? '' : applied, name, connectionOf(config))));
      if (id !== operation.current) return;
      if (!result.success) { setNotice({ message: result.message, error: true }); return; }
      setProfiles(result.profiles);
      if (kind === 'apply') {
        const next = configOf(result.config);
        onReplace(next); remember(next); setRestored(true); setApplied(result.selectedID); setCandidate(result.selectedID);
      } else if (kind === 'delete') {
        if (target === applied) {
          const fallback = result.config ? result : responseOf(await ProfileService.Apply(''));
          if (id !== operation.current) return;
          if (!fallback.success) { setNotice({ message: fallback.message, error: true }); setProfiles(result.profiles); return; }
          const next = configOf(fallback.config); onReplace(next); remember(next); setRestored(true); setApplied(''); setCandidate('');
        }
        else setCandidate(applied);
      } else {
        setApplied(result.selectedID); setCandidate(result.selectedID); remember(connectionOf(config));
      }
      setNotice({ message: result.message, error: !!result.warning }); setDialog(null);
    } catch { if (id === operation.current) setNotice({ message: 'Profile operation failed. Check local profile file access and retry; current inputs were kept.', error: true }); }
    finally { if (id === operation.current) { setBusy(false); onBusy(false); } }
  }
  function apply() {
    if (dirty) setDialog({ kind: 'apply' }); else void run('apply');
  }
  const locked = busy || autoSaving;
  const status = busy ? 'Loading' : autoSaving ? 'Saving' : notice.error ? 'Attention' : pending ? 'Not applied' : dirty ? 'Unsaved' : 'Saved';
  return <section className="registry-disclosure profile-section">
    <div className="registry-divider">
      <Button size="compact-xs" variant="transparent" color="gray" aria-expanded={open} aria-controls="registry-profiles"
        leftSection={<Icon name="connection" size={13} />} onClick={() => setOpen((value) => !value)}>{open ? '▾' : '▸'} Profile</Button>
      <Badge size="xs" variant="light" color={notice.error ? 'red' : dirty ? 'orange' : 'green'} aria-label="Profile status" title={notice.message}>
        {activeProfile ? (activeProfile.name.toLowerCase() === 'default profile' ? `${activeProfile.name} (named)` : activeProfile.name) : 'Default Profile'} · {status}
      </Badge>
    </div>
    <Collapse expanded={open} id="registry-profiles" keepMountedMode="display-none" transitionDuration={0}>
    <div className="profile-toolbar">
    <Group gap={6} wrap="wrap">
      <NativeSelect size="xs" aria-label="Profile" className="profile-select" value={candidate} disabled={locked}
        data={[{ value: '', label: 'Default Profile' }, ...profiles.map((p) => ({ value: p.id, label: p.name.toLowerCase() === 'default profile' ? `${p.name} (named)` : p.name }))]}
        onChange={(e) => { setCandidate(e.target.value); onInvalidate(); setNotice({ message: 'Selection changed. Apply replaces the complete connection; current inputs are kept until then.', error: false }); }} />
      <Button size="compact-xs" variant="light" disabled={locked} onClick={apply}>Apply</Button>
      <Button size="compact-xs" variant="subtle" leftSection={<Icon name="save" />} disabled={locked || pending || (!applied && (!restored || !dirty || !notice.error))} onClick={() => applied ? void run('save', applied, activeProfile?.name || '') : void persistDefault(connectionOf(config), passwordRevision)}>Save</Button>
      <Button size="compact-xs" variant="subtle" leftSection={<Icon name="plus" />} disabled={locked} onClick={() => { setNotice({ message: '', error: false }); setDialog({ kind: 'saveAs', name: '' }); }}>Save As</Button>
      <Button size="compact-xs" variant="subtle" color="red" leftSection={<Icon name="trash" />} disabled={locked || !candidate} onClick={() => setDialog({ kind: 'delete', target: candidate })}>Delete</Button>
      {dirty && <Badge size="xs" color="orange" variant="light">Unsaved changes</Badge>}
      {pending && <Badge size="xs" variant="outline">Not applied</Badge>}
    </Group>
    {(busy || notice.message) && <Text size="xs" mt={4} c={notice.error ? 'red' : 'dimmed'} role="status" className="profile-notice">{busy ? 'Working with local profiles…' : notice.message}</Text>}
    </div>
    </Collapse>
    <Modal opened={!!dialog} onClose={() => { if (!busy) setDialog(null); }} title={dialog?.kind === 'saveAs' ? 'Save connection as profile' : dialog?.kind === 'delete' ? 'Delete profile?' : 'Replace unsaved connection?'} size="sm" centered transitionProps={{ duration: 0 }}>
      {dialog?.kind === 'saveAs' ? <TextInput label="Profile Name" aria-label="Profile Name" data-autofocus value={dialog.name} disabled={locked}
        onChange={(e) => setDialog({ ...dialog, name: e.target.value })}
        onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void run('saveAs', '', dialog.name); } }} />
        : <Text size="sm">{dialog?.kind === 'delete' ? 'Remove the complete saved local connection, including its password. If this is the active profile, its saved connection is replaced by Default Profile; Topic Name is kept.' : 'Applying replaces the URL, both SSL paths and password. Unsaved connection changes will be discarded. Topic Name is kept.'}</Text>}
      {notice.error && <Text size="xs" c="red" mt="xs" role="alert">{notice.message}</Text>}
      <Group justify="end" gap="xs" mt="md">
        <Button size="xs" variant="default" disabled={locked} onClick={() => setDialog(null)}>Cancel</Button>
        <Button size="xs" color={dialog?.kind === 'delete' ? 'red' : 'blue'} disabled={locked} onClick={() => void run(dialog.kind === 'saveAs' ? 'saveAs' : dialog.kind, dialog.target ?? candidate, dialog.name || '')}>
          {dialog?.kind === 'delete' ? 'Delete profile' : dialog?.kind === 'saveAs' ? 'Save profile' : 'Discard and apply'}
        </Button>
      </Group>
    </Modal>
  </section>;
}
