import { useEffect, useMemo, useRef, useState } from 'react';
import { Button, Group, Paper, Text, TextInput, Textarea } from '@mantine/core';
import { Clipboard } from '@wailsio/runtime';

function CopyIcon() {
  return <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true">
    <rect x="8" y="8" width="12" height="12" rx="2" />
    <path d="M16 8V4a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h4" />
  </svg>;
}

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || '');
// Cmd+F on macOS, Ctrl+F elsewhere.
const isFindShortcut = (e) => e.key?.toLowerCase() === 'f' && !e.altKey && !e.shiftKey && (isMac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey);
const composing = (e) => e.nativeEvent?.isComposing || e.keyCode === 229;

// Literal match positions; case-insensitive unless lowercasing would change
// string lengths (then indices would not map back, so match case-sensitively).
export function findMatches(text, query) {
  if (!query) return [];
  let hay = text, needle = query;
  const lowerText = text.toLowerCase(), lowerQuery = query.toLowerCase();
  if (lowerText.length === text.length && lowerQuery.length === query.length) { hay = lowerText; needle = lowerQuery; }
  const found = [];
  for (let i = hay.indexOf(needle); i !== -1; i = hay.indexOf(needle, i + needle.length)) found.push(i);
  return found;
}

export default function Editor({ name, value, readOnly = false, canCopy = true, busy = false, onChange, headerDetail, placeholder, searchable = false }) {
  const [copyState, setCopyState] = useState('idle');
  const copyId = useRef(0);
  const textarea = useRef(null);
  const findInput = useRef(null);
  const [find, setFind] = useState({ open: false, query: '', index: -1 });
  const matches = useMemo(() => (find.open ? findMatches(value, find.query) : []), [find.open, value, find.query]);
  const current = matches.length ? Math.min(Math.max(find.index, 0), matches.length - 1) : -1;
  const [focusFind, setFocusFind] = useState(0);
  useEffect(() => { if (focusFind) { findInput.current?.focus(); findInput.current?.select(); } }, [focusFind]);
  useEffect(() => {
    copyId.current += 1;
    setCopyState('idle');
    return () => { copyId.current += 1; };
  }, [value, canCopy]);
  useEffect(() => {
    if (copyState !== 'copied' && copyState !== 'error') return;
    const timer = setTimeout(() => setCopyState('idle'), 2200);
    return () => clearTimeout(timer);
  }, [copyState]);

  async function copy() {
    const id = ++copyId.current;
    setCopyState('copying');
    try {
      await Clipboard.SetText(value);
      if (id === copyId.current) setCopyState('copied');
    } catch {
      if (id === copyId.current) setCopyState('error');
    }
  }

  // Selects a match with the native selection API (text and undo history are
  // untouched). Briefly focusing the textarea lets the browser scroll the
  // selection into view; focus then returns to the find field.
  function reveal(start, length) {
    const element = textarea.current;
    if (!element || start < 0) return;
    element.focus({ preventScroll: true });
    element.setSelectionRange(start, start + length);
    element.blur(); element.focus();
    findInput.current?.focus({ preventScroll: true });
  }
  function firstFrom(found) {
    const from = textarea.current ? textarea.current.selectionStart : 0;
    return found.length ? Math.max(0, found.findIndex((at) => at >= from)) : -1;
  }
  function openFind() {
    const element = textarea.current;
    const selected = element ? element.value.slice(element.selectionStart, element.selectionEnd) : '';
    const query = selected && !selected.includes('\n') && selected.length <= 200 ? selected : find.query;
    setFind({ open: true, query, index: firstFrom(findMatches(value, query)) });
    setFocusFind((n) => n + 1);
  }
  function closeFind() {
    setFind((state) => ({ ...state, open: false }));
    textarea.current?.focus();
  }
  function changeQuery(query) {
    const found = findMatches(value, query);
    const index = firstFrom(found);
    setFind({ open: true, query, index });
    if (index >= 0) reveal(found[index], query.length);
  }
  function step(direction) {
    if (!matches.length) return;
    const index = (current + direction + matches.length) % matches.length; // wraps around
    setFind((state) => ({ ...state, index }));
    reveal(matches[index], find.query.length);
  }
  function onEditorKeyDown(e) {
    if (!isFindShortcut(e) || composing(e)) return;
    e.preventDefault();
    if (e.target === findInput.current) findInput.current.select();
    else if (e.target === textarea.current) openFind();
  }
  function onFindKeyDown(e) {
    if (composing(e)) return;
    if (e.key === 'Enter') { e.preventDefault(); step(e.shiftKey ? -1 : 1); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeFind(); }
  }

  const id = `${name.toLowerCase().replaceAll(' ', '-')}-editor`;
  return (
    <Paper withBorder radius="md" className="editor" onKeyDown={searchable ? onEditorKeyDown : undefined}>
      <Group className="editor-header" justify="space-between" gap="xs" wrap="nowrap">
        <Text component="label" htmlFor={id} size="xs" fw={600}>{name}</Text>
        {headerDetail && <div className="editor-header-detail">{headerDetail}</div>}
        <Group gap={6} wrap="nowrap">
          <Text size="xs" c="red" role="status" title={copyState === 'error' ? 'Select text and use Cmd/Ctrl+C to copy.' : undefined}>{copyState === 'error' ? 'Copy failed' : ''}</Text>
          <Button size="compact-xs" variant="subtle" leftSection={<CopyIcon />}
            aria-label={`Copy ${name}`} disabled={!canCopy || copyState === 'copying'}
            color={copyState === 'copied' ? 'green' : 'gray'} onClick={() => void copy()}>
            {copyState === 'copied' ? 'Copied' : 'Copy'}
          </Button>
        </Group>
      </Group>
      {searchable && find.open && <Group gap={4} wrap="nowrap" className="editor-find" role="search">
        <TextInput size="xs" ref={findInput} className="editor-find-input" aria-label={`Find in ${name}`} placeholder="Find" value={find.query}
          spellCheck={false} autoComplete="off" onChange={(e) => changeQuery(e.target.value)} onKeyDown={onFindKeyDown} />
        <Text size="xs" c={find.query && !matches.length ? 'red' : 'dimmed'} className="editor-find-count" aria-live="polite">
          {!find.query ? '' : matches.length ? `${current + 1}/${matches.length}` : 'No results'}
        </Text>
        <Button size="compact-xs" variant="subtle" color="gray" aria-label="Previous match" disabled={!matches.length} onClick={() => step(-1)}>↑</Button>
        <Button size="compact-xs" variant="subtle" color="gray" aria-label="Next match" disabled={!matches.length} onClick={() => step(1)}>↓</Button>
        <Button size="compact-xs" variant="subtle" color="gray" aria-label="Close find" onClick={closeFind}>×</Button>
      </Group>}
      <Textarea id={id} ref={textarea} value={value} readOnly={readOnly}
        onChange={onChange} spellCheck={false} autoCapitalize="off" autoCorrect="off"
        aria-busy={busy} variant="unstyled"
        classNames={{ root: 'editor-field', wrapper: 'editor-wrapper', input: 'editor-input' }}
        placeholder={placeholder ?? (readOnly ? 'Processed text appears here' : 'Enter text to process…')} />
    </Paper>
  );
}
