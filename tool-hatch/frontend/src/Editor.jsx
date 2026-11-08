import { useEffect, useRef, useState } from 'react';
import { Button, Group, Paper, Text, Textarea } from '@mantine/core';
import { Clipboard } from '@wailsio/runtime';

function CopyIcon() {
  return <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true">
    <rect x="8" y="8" width="12" height="12" rx="2" />
    <path d="M16 8V4a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h4" />
  </svg>;
}

export default function Editor({ name, value, readOnly = false, canCopy = true, busy = false, onChange, headerDetail, placeholder }) {
  const [copyState, setCopyState] = useState('idle');
  const copyId = useRef(0);
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

  return (
    <Paper withBorder radius="md" className="editor">
      <Group className="editor-header" justify="space-between" gap="xs" wrap="nowrap">
        <Text component="label" htmlFor={`${name.toLowerCase().replaceAll(' ', '-')}-editor`} size="xs" fw={600}>{name}</Text>
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
      <Textarea id={`${name.toLowerCase().replaceAll(' ', '-')}-editor`} value={value} readOnly={readOnly}
        onChange={onChange} spellCheck={false} autoCapitalize="off" autoCorrect="off"
        aria-busy={busy} variant="unstyled"
        classNames={{ root: 'editor-field', wrapper: 'editor-wrapper', input: 'editor-input' }}
        placeholder={placeholder ?? (readOnly ? 'Processed text appears here' : 'Enter text to process…')} />
    </Paper>
  );
}
