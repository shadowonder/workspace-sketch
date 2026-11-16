const paths = {
  connection: 'M8 4H5a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h3m4-12h3a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-3M7 10h6m-2-2 2 2-2 2',
  shield: 'M10 2 3 5v5c0 4 7 8 7 8s7-4 7-8V5l-7-3Zm-3 8 2 2 4-4',
  fetch: 'M16 7a6 6 0 1 0 0 6M16 3v4h-4',
  save: 'M4 3h10l3 3v11H3V3h1Zm2 0v5h7V3M6 17v-6h8v6',
  plus: 'M10 4v12M4 10h12',
  trash: 'M3 5h14M7 5V3h6v2M5 5l1 12h8l1-12M8 8v6m4-6v6',
  file: 'M5 2h7l4 4v12H4V2h1Zm6 0v5h5M7 11h6m-6 3h4',
  check: 'm4 10 4 4 8-8',
  alert: 'M10 3 1 17h18L10 3Zm0 5v4m0 2v1',
  chevron: 'm7 4 6 6-6 6',
  play: 'M6 4v12l10-6L6 4Z',
  stop: 'M5 5h10v10H5z',
};
export default function RegistryIcon({ name, size = 15 }) {
  return <svg width={size} height={size} viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={paths[name] || paths.file} /></svg>;
}
