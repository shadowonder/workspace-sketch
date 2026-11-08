import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Badge, Button, Checkbox, Group, Loader, MantineProvider, NativeSelect,
  Tabs, Text, TextInput, useMantineColorScheme,
} from '@mantine/core';
import Editor from './Editor.jsx';
import SchemaRegistry from './SchemaRegistry.jsx';
import { StringService } from '../bindings/github.com/shadowonder/fast-chrome-plugin/tool-hatch';

const tools = [
  { id: 'string', label: 'String', component: StringTool },
  { id: 'schema-registry', label: 'Schema Registry', component: SchemaRegistry },
];
const tabStorageKey = 'toolhatch.active-tab';
function savedTab() {
  try {
    const id = window.localStorage.getItem(tabStorageKey);
    return tools.some((tool) => tool.id === id) ? id : 'string';
  } catch { return 'string'; }
}
const operations = [{ value: 'avro-to-plain', label: 'Avro JSON → Plain JSON' }];
const theme = {
  primaryColor: 'blue',
  defaultRadius: 'sm',
  fontFamily: 'system-ui, sans-serif',
  fontFamilyMonospace: 'ui-monospace, SFMono-Regular, Consolas, monospace',
};

export default function App() {
  return (
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <Tools />
    </MantineProvider>
  );
}

function Tools() {
  const [activeTool, setActiveTool] = useState(savedTab);
  const [registryVisited, setRegistryVisited] = useState(() => savedTab() === 'schema-registry');
  useEffect(() => {
    try { window.localStorage.setItem(tabStorageKey, activeTool); } catch { /* Preference storage is optional. */ }
  }, [activeTool]);
  const { colorScheme, setColorScheme } = useMantineColorScheme();

  return (
    <Tabs className="app" value={activeTool} onChange={(id) => { if (tools.some((tool) => tool.id === id)) { if (id === 'schema-registry') setRegistryVisited(true); setActiveTool(id); } }} keepMounted={false} keepMountedMode="display-none">
      <header className="app-header">
        <Group gap="md" wrap="nowrap">
          <Text size="xs" c="dimmed" fw={600}>TOOLS</Text>
          <Tabs.List aria-label="Tools">
            {tools.map((tool) => <Tabs.Tab key={tool.id} value={tool.id}>{tool.label}</Tabs.Tab>)}
          </Tabs.List>
        </Group>
        <NativeSelect size="xs" w={100} aria-label="Color theme"
          value={colorScheme} onChange={(event) => setColorScheme(event.target.value)}
          data={[{ value: 'auto', label: 'System' }, { value: 'light', label: 'Light' }, { value: 'dark', label: 'Dark' }]} />
      </header>
      {tools.map(({ id, component: Tool }) => (
        <Tabs.Panel key={id} value={id} className="tool-panel" keepMounted={id === 'schema-registry' && registryVisited}><Tool {...(id === 'schema-registry' ? { active: activeTool === id } : {})} /></Tabs.Panel>
      ))}
    </Tabs>
  );
}

function StringTool() {
  const [input, setInput] = useState('');
  const [namespacePrefix, setNamespacePrefix] = useState('');
  const [operation, setOperation] = useState('avro-to-plain');
  const [manual, setManual] = useState(false);
  const [state, setState] = useState({ phase: 'idle', response: null, error: '' });
  const requestId = useRef(0);

  const process = useCallback(async (text, selectedOperation, prefix) => {
    const id = ++requestId.current;
    setState({ phase: 'processing', response: null, error: '' });
    try {
      const response = await StringService.Process(text, selectedOperation, prefix);
      if (id !== requestId.current) return;
      if (
        typeof response?.result !== 'string' ||
        typeof response?.metadata?.success !== 'boolean' ||
        typeof response?.metadata?.message !== 'string' ||
        response.metadata.operation !== selectedOperation ||
        (response.metadata.warnings !== undefined &&
          (!Array.isArray(response.metadata.warnings) ||
            !response.metadata.warnings.every((warning) => typeof warning === 'string')))
      ) {
        throw new Error('Invalid processing response.');
      }
      setState({ phase: 'complete', response, error: '' });
    } catch {
      if (id !== requestId.current) return;
      setState({
        phase: 'error',
        response: null,
        error: 'Could not process the string. Please try again.',
      });
    }
  }, []);

  useEffect(() => {
    if (!manual) void process(input, operation, namespacePrefix);
    // Invalidate automatic and manual requests on edits, mode changes or unmount.
    return () => { requestId.current += 1; };
  }, [input, operation, namespacePrefix, manual, process]);

  function invalidate() {
    // Also invalidate synchronously, before a queued response or the next effect.
    requestId.current += 1;
    setState({ phase: 'idle', response: null, error: '' });
  }

  const failed = state.phase === 'error' || state.response?.metadata.success === false;
  const done = state.response?.metadata.success === true;
  const message = failed
    ? state.error || state.response.metadata.message || 'Processing failed.'
    : done
      ? 'Done'
      : state.phase === 'processing'
        ? 'Processing…'
        : manual ? 'Ready — click Apply.' : 'Processing…';

  return (
    <div className="string-tool">
      <Group className="toolbar" gap="sm" align="end" wrap="nowrap" role="group" aria-label="String operations">
        <NativeSelect size="xs" label="Operation" aria-label="Operation" w={230}
          value={operation} data={operations} onChange={(event) => {
            invalidate();
            setOperation(event.target.value);
          }} />
        <Button size="xs" className="apply" disabled={!manual} onClick={() => void process(input, operation, namespacePrefix)}>
          Apply
        </Button>
        <Checkbox size="xs" className="manual" label="Manual apply" checked={manual}
          onChange={(event) => {
            invalidate();
            setManual(event.target.checked);
          }} />
      </Group>
      <div className="editors">
        <div className="input-column">
          <Editor name="Input" value={input} onChange={(event) => {
            invalidate();
            setInput(event.target.value);
          }} />
          <TextInput size="xs" label="Namespace Prefix" aria-label="Namespace Prefix" placeholder="com.example."
            description="Optional. Empty uses automatic named-type detection."
            value={namespacePrefix} spellCheck={false} autoCapitalize="off" autoCorrect="off"
            onChange={(event) => {
              invalidate();
              setNamespacePrefix(event.target.value);
            }} />
        </div>
        <Editor name="Output" value={state.response?.result ?? ''} readOnly
          canCopy={done} busy={state.phase === 'processing'} />
      </div>
      <Group gap={8} className={`status ${failed ? 'error' : done ? 'done' : ''}`} role="status" aria-live="polite">
        {state.phase === 'processing'
          ? <Loader size={12} aria-hidden="true" />
          : <Badge size="xs" variant="dot" color={failed ? 'red' : done ? 'green' : 'gray'} aria-hidden="true" />}
        <Text size="xs" c={failed ? 'red' : done ? 'green' : 'dimmed'}>{message}</Text>
      </Group>
      {state.response?.metadata.warnings?.length > 0 && (
        <div className="conversion-warnings" role="note" aria-label="Conversion warnings">
          {state.response.metadata.warnings.map((warning, index) => (
            <Text key={index} size="xs" c="dimmed">{warning}</Text>
          ))}
        </div>
      )}
    </div>
  );
}
