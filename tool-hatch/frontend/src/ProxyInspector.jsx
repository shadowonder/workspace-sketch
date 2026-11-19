import { memo, useEffect, useRef, useState } from 'react';
import { Badge, Button, Paper, Text } from '@mantine/core';
import { ProxyService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';

// Debug captures stay in Go. The Inspector fetches only previews and sizes for
// the selected entry (Capture), and Copy asks Go to put the complete retained
// content on the clipboard (CopyCapture), so large bodies never pass through React.
export function formatSize(bytes) {
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MiB`;
  if (bytes >= 1 << 10) return `${(bytes / (1 << 10)).toFixed(1)} KiB`;
  return `${bytes} B`;
}
const stateLabel = { complete: ['Complete', 'green'], capturing: ['Capturing…', 'blue'], incomplete: ['Incomplete', 'yellow'], released: ['Released', 'gray'], none: ['Not captured', 'gray'] };
function validView(view) {
  if (!view || typeof view.success !== 'boolean' || typeof view.state !== 'string' || !view.request || !view.response) throw new Error('Invalid bridge response');
  return view;
}

function Part({ title, part, onCopy, copy }) {
  if (part.state === 'none') {
    return <div className="inspector-part"><Text size="xs" fw={600}>{title}</Text><Text size="xs" c="dimmed">{part.note || 'Not captured.'}</Text></div>;
  }
  const partial = part.state !== 'complete';
  return <div className="inspector-part">
    <div className="inspector-part-heading">
      <Text size="xs" fw={600}>{title}</Text>
      <Text size="xs" c="dimmed" className="inspector-size">
        {formatSize(part.size)}{partial && part.observed > part.size ? ` of ${formatSize(part.observed)} seen` : ''}
      </Text>
      {partial && <Badge size="xs" variant="light" color={stateLabel[part.state]?.[1]}>{stateLabel[part.state]?.[0]}</Badge>}
      <Text size="xs" c={copy?.error ? 'red' : 'dimmed'} className="inspector-copy-result" role="status">{copy?.message ?? ''}</Text>
      <Button size="compact-xs" variant="subtle" color="gray" aria-label={`Copy ${title}`} disabled={copy?.busy} onClick={onCopy}>Copy</Button>
    </div>
    {part.note && <Text size="xs" c={part.state === 'incomplete' ? 'yellow' : 'dimmed'} className="inspector-note">{part.note}</Text>}
    {part.binary
      ? <Text size="xs" c="dimmed">Binary or compressed content — Copy copies it as base64.</Text>
      : part.preview ? <pre className="inspector-preview">{part.preview}</pre> : <Text size="xs" c="dimmed">(empty)</Text>}
    {part.truncated && !part.binary && <Text size="xs" c="dimmed" className="inspector-note">Preview shows the first {formatSize(part.preview.length)}; Copy copies all {formatSize(part.size)}.</Text>}
  </div>;
}

function ProxyInspector({ entry = null, selectedId = 0, open, onToggle, refresh = 0, debugBytes = 0, debugLimit = 0 }) {
  const [view, setView] = useState(null);
  const [copies, setCopies] = useState({});
  const request = useRef(0);
  const selection = useRef(selectedId);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const id = entry?.id ?? 0;

  useEffect(() => { selection.current = selectedId; setView(null); setCopies({}); }, [selectedId]);
  // Refetch when the selection, its completion/status or retained Debug memory
  // changes. The existing Status poll also refreshes pending captures, whose
  // contents can change without allocating any more capture memory.
  useEffect(() => {
    if (!open || !id) return;
    const mine = ++request.current;
    ProxyService.Capture(id).then(validView).then((next) => {
      if (mounted.current && mine === request.current) setView(next);
    }).catch(() => {
      if (mounted.current && mine === request.current) setView({ success: false, message: 'Could not load the capture.', state: 'none', request: {}, response: {} });
    });
  }, [open, id, entry?.pending, entry?.status, debugBytes, refresh]);

  async function copy(part) {
    const mine = selection.current;
    setCopies((all) => ({ ...all, [part]: { busy: true, message: 'Copying…' } }));
    let result;
    try { result = await ProxyService.CopyCapture(id, part); } catch { result = { success: false, message: 'Copy failed.' }; }
    if (!mounted.current || mine !== selection.current) return; // selection changed meanwhile
    setCopies((all) => ({ ...all, [part]: { busy: false, error: !result?.success, message: result?.message ?? 'Copy failed.' } }));
  }

  const state = view?.success ? stateLabel[view.state] : null;
  return <Paper withBorder radius="md" className="proxy-inspector" data-open={open} aria-label="Request inspector">
    <div className="versions-heading proxy-console-heading">
      <Button size="compact-xs" variant="transparent" color="gray" aria-expanded={open} aria-controls="proxy-inspector-body" onClick={onToggle}>
        {open ? '▾' : '▸'} Inspector
      </Button>
      <Text size="xs" c="dimmed" className="inspector-summary">{entry ? `#${entry.id} ${entry.method} ${entry.path}` : ''}</Text>
      {open && state && <Badge size="xs" variant="light" color={state[1]} aria-label="Capture state">{state[0]}</Badge>}
      {debugLimit > 0 && <Text size="xs" c="dimmed" className="inspector-memory" title="Debug capture memory in use / budget">{formatSize(debugBytes)} / {formatSize(debugLimit)}</Text>}
    </div>
    {open && <div id="proxy-inspector-body" className="inspector-body">
      {!selectedId && <Text size="xs" c="dimmed" className="console-note">Turn on Debug, send requests, then select a DEBUG entry in Activity.</Text>}
      {selectedId > 0 && !entry && <Text size="xs" c="dimmed" className="console-note">This entry is no longer retained.</Text>}
      {entry && !view && <Text size="xs" c="dimmed" className="console-note">Loading…</Text>}
      {entry && view && !view.success && <Text size="xs" c="red" className="console-note">{view.message}</Text>}
      {entry && view?.success && <>
        <Text size="xs" className="inspector-meta">
          {entry.method} {entry.path} · {entry.pending ? 'in progress' : `${entry.status || 'ERR'} · ${entry.durationMs} ms`} · started {entry.time.slice(11, 19)}
        </Text>
        {view.note && <Text size="xs" c={view.state === 'released' || view.state === 'none' ? 'dimmed' : 'yellow'} className="inspector-note">{view.note}</Text>}
        {view.state !== 'released' && view.state !== 'none' && <>
          <Text size="xs" c="dimmed" className="inspector-line">→ {view.request.line}</Text>
          <Part title="Request headers" part={view.request.headers} copy={copies.requestHeaders} onCopy={() => void copy('requestHeaders')} />
          <Part title="Request body" part={view.request.body} copy={copies.requestBody} onCopy={() => void copy('requestBody')} />
          <Text size="xs" c="dimmed" className="inspector-line">← {view.response.line || 'no response'}</Text>
          <Part title="Response headers" part={view.response.headers} copy={copies.responseHeaders} onCopy={() => void copy('responseHeaders')} />
          <Part title="Response body" part={view.response.body} copy={copies.responseBody} onCopy={() => void copy('responseBody')} />
        </>}
      </>}
    </div>}
  </Paper>;
}

export default memo(ProxyInspector);
