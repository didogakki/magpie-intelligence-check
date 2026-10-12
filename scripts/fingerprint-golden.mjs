// Generate deterministic synthetic answers for the Go/JavaScript scorer parity test.
// No archived probe answers or account data are used.
//
//   node scripts/fingerprint-golden.mjs
import { readFileSync, writeFileSync } from 'node:fs';
import { analyzeGlobalOutputs } from '../internal/check/web/modeltrace/fingerprint-core.js';

const bank = JSON.parse(readFileSync(new URL('../internal/check/web/modeltrace/unified_bank.json', import.meta.url)));
const destination = new URL('../internal/check/testdata/fingerprint_golden.json', import.meta.url);

function numbers(seed, count) {
  let state = seed >>> 0;
  return Array.from({ length: count }, () => {
    state ^= state << 13;
    state ^= state >>> 17;
    state ^= state << 5;
    return (state >>> 0) % 355 + 1;
  });
}

const cases = [];
for (let index = 0; index < 16; index++) {
  const outputs = [0, 1, 2].map((part) => ({
    text: numbers(0x6d2b79f5 ^ ((index + 1) * 7919) ^ (part * 104729), 300 + part * 7).join(', '),
    expected_count: 300 + part * 7,
  }));
  const report = analyzeGlobalOutputs(outputs, bank);
  cases.push({
    id: `synthetic-${index + 1}`,
    outputs,
    js: {
      prediction: report.prediction,
      probability: report.probability,
      used_outputs: report.used_outputs,
      top: report.results.slice(0, 5).map(r => ({ model: r.model, probability: r.probability, score: r.score })),
      parsed: report.diagnostics.map(d => d.parsed_numbers),
    },
  });
}
writeFileSync(destination, JSON.stringify({ bank_built_at: bank.built_at, cases }, null, 1) + '\n');
console.log(`${cases.length} synthetic cases`);
