import { copyFile, mkdir } from 'node:fs/promises';

const folder = new URL('../ui/vendor/', import.meta.url);
await mkdir(folder, { recursive: true });
const xterm = new URL('../node_modules/@xterm/xterm/', import.meta.url);
for (const [source, target] of [
  ['lib/xterm.js', 'xterm.js'],
  ['css/xterm.css', 'xterm.css'],
  ['LICENSE', 'xterm.LICENSE'],
]) {
  await copyFile(new URL(source, xterm), new URL(target, folder));
}
