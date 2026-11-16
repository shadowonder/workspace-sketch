import { memo, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Badge, Button, Paper, Text } from '@mantine/core';
import { ProxyService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';
import Icon from './RegistryIcon.jsx';

// Script console output lives in a bounded Go memory buffer. The existing
// Status poll carries consoleLastID; entries are fetched incrementally only
// when it advances, so an idle console causes no extra calls or renders.
const keep = 300;
const levelColor = { error: 'red', warn: 'yellow', debug: 'dimmed', info: 'blue', log: undefined };
function validConsole(response) {
  if (!response || !Array.isArray(response.entries) || !Number.isInteger(response.lastID) || typeof response.truncated !== 'boolean') throw new Error('Invalid bridge response');
  return response;
}

function ProxyConsole({ lastID = 0 }) {
  const [open, setOpen] = useState(false);
  const [log, setLog] = useState({ entries: [], truncated: false });
  const cursor = useRef(0);
  const fetching = useRef(false);
  const again = useRef(false);
  const clearEpoch = useRef(0);
  const mounted = useRef(true);
  const list = useRef(null);
  const atBottom = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  const fetchNew = useCallback(async () => {
    if (fetching.current) { again.current = true; return; }
    fetching.current = true;
    try {
      do {
        again.current = false;
        const epoch = clearEpoch.current;
        const response = validConsole(await ProxyService.Console(cursor.current));
        if (!mounted.current || epoch !== clearEpoch.current) continue; // a Clear superseded this fetch
        cursor.current = Math.max(cursor.current, response.lastID);
        if (response.entries.length || response.truncated) {
          setLog((previous) => ({
            entries: [...previous.entries, ...response.entries].slice(-keep),
            truncated: previous.truncated || response.truncated,
          }));
        }
      } while (again.current && mounted.current);
    } catch { /* The next status poll retries. */ } finally { fetching.current = false; }
  }, []);

  useEffect(() => { if (lastID > cursor.current) void fetchNew(); }, [lastID, fetchNew]);

  async function clear() {
    clearEpoch.current += 1;
    setLog({ entries: [], truncated: false });
    try {
      const response = validConsole(await ProxyService.ClearConsole());
      cursor.current = Math.max(cursor.current, response.lastID);
    } catch { /* Local view is cleared; Go keeps its bounded buffer. */ }
  }

  // Follow new output only while the user is scrolled to the bottom.
  useLayoutEffect(() => {
    if (open && list.current && atBottom.current) list.current.scrollTop = list.current.scrollHeight;
  }, [open, log]);

  return <Paper withBorder radius="md" className="proxy-console" data-open={open} aria-label="Script console">
    <div className="versions-heading proxy-console-heading">
      <Button size="compact-xs" variant="transparent" color="gray" aria-expanded={open} aria-controls="proxy-console-output"
        onClick={() => setOpen((value) => !value)}>{open ? '▾' : '▸'} Console</Button>
      <Badge size="xs" variant="light" color="gray" aria-label="Console entry count">{log.entries.length}</Badge>
      <Button size="compact-xs" variant="subtle" color="gray" className="proxy-console-clear" leftSection={<Icon name="trash" size={12} />}
        disabled={!log.entries.length && !log.truncated} onClick={() => void clear()}>Clear</Button>
    </div>
    {open && <div id="proxy-console-output" className="console-list" ref={list} role="log" aria-live="off"
      onScroll={(event) => { const e = event.currentTarget; atBottom.current = e.scrollHeight - e.scrollTop - e.clientHeight < 8; }}>
      {log.truncated && <Text size="xs" c="dimmed" className="console-note">Older entries were discarded (last {keep} kept).</Text>}
      {log.entries.map((entry) => <div key={entry.id} className={`console-entry console-${entry.level}`}>
        <span className="console-time">{entry.time.slice(11, 23)}</span>
        <Text component="span" size="xs" fw={600} c={levelColor[entry.level]} className="console-level">{entry.level}</Text>
        <Text component="span" size="xs" c={entry.level === 'error' || entry.level === 'warn' ? levelColor[entry.level] : undefined} className="console-message">{entry.message}</Text>
      </div>)}
      {!log.entries.length && <Text size="xs" c="dimmed" className="console-note">Use console.log(…) in the script; output stays in memory only.</Text>}
    </div>}
  </Paper>;
}

export default memo(ProxyConsole);
