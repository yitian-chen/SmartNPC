'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { validate } = require('./provider.cjs');
function prepare(reportFile, outputFile) {
  const reportPath = path.resolve(reportFile);
  const report = validate(JSON.parse(fs.readFileSync(reportPath, 'utf8')));
  const groups = [...new Set(report.results.map(r => r.group))].sort();
  const samples = new Map(report.results.map(r => [JSON.stringify([r.case, r.repeat]), r]));
  const config = {
    description: `Memory bench ${report.mode.toUpperCase()} | ${report.started} | ${report.suite_sha256}`,
    prompts: ['Case {{case_id}}, repeat {{repeat}}'],
    providers: groups.map(group => ({ id: `file://${path.join(__dirname, 'provider.cjs')}`,
      label: `${group} (${report.mode})`, config: { report: reportPath, group } })),
    tests: [...samples.values()].map(r => ({ description: `${r.category}: ${r.case} #${r.repeat}`,
      vars: { case_id: r.case, repeat: r.repeat }, metadata: { category: r.category, mode: report.mode } })),
    defaultTest: { assert: [{ type: 'javascript', metric: report.mode === 'live' ? 'ScenarioPass' : 'OfflinePipelineOnly',
      value: `file://${path.join(__dirname, 'assertion.cjs')}` }] },
    evaluateOptions: { maxConcurrency: 1, cache: false }
  };
  fs.mkdirSync(path.dirname(path.resolve(outputFile)), { recursive: true });
  fs.writeFileSync(outputFile, JSON.stringify(config, null, 2), { mode: 0o600 });
  return config;
}
module.exports = { prepare };
if (require.main === module) {
  if (process.argv.length !== 4) throw new Error('Usage: prepare.cjs results.json config.json');
  prepare(process.argv[2], process.argv[3]);
}
