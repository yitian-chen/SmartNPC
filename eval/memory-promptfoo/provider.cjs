'use strict';
const fs = require('node:fs');

function validate(report) {
  if (!['live', 'offline'].includes(report.mode) || !Array.isArray(report.results) || !report.results.length) {
    throw new Error('Not a non-empty memory bench report');
  }
  const keys = new Set();
  for (const r of report.results) {
    const key = JSON.stringify([r.case, r.repeat, r.group]);
    if (!r.case || !Number.isInteger(r.repeat) || !['memory', 'no-memory'].includes(r.group) || keys.has(key) ||
        !['passed', 'ability_failed', 'offline_validated', 'service_error', 'budget_stopped', 'invalid_fixture'].includes(r.status) ||
        !Array.isArray(r.checks)) throw new Error('Invalid or duplicate bench result');
    keys.add(key);
  }
  return report;
}

function stageScores(r) {
  return Object.fromEntries(['write', 'update', 'read', 'use'].map(stage => {
    const checks = r.checks.filter(c => c.applicable && c.stage === stage);
    return [stage, { passed: checks.filter(c => c.pass).length, total: checks.length }];
  }));
}

class MemoryBenchProvider {
  constructor(options) {
    this.config = options.config;
    this.report = validate(JSON.parse(fs.readFileSync(this.config.report, 'utf8')));
  }
  id() { return `memory-bench:${this.config.group}`; }
  async callApi(_prompt, context) {
    const r = this.report.results.find(r => r.case === context.vars.case_id && r.repeat === context.vars.repeat && r.group === this.config.group);
    if (!r) return { error: 'This case/repeat/group did not run; not a passing sample' };
    if (['service_error', 'budget_stopped', 'invalid_fixture'].includes(r.status)) {
      return { error: `${r.status}: ${r.error || 'See the Go bench report'}` };
    }
    if (this.report.mode === 'live' && r.status === 'offline_validated') return { error: 'Offline result in live report' };
    const stage = stageScores(r);
    const calls = r.calls || [];
    const prompt = calls.reduce((s, c) => s + (c.input_tokens || 0), 0);
    const completion = calls.reduce((s, c) => s + (c.output_tokens || 0), 0);
    return {
      output: JSON.stringify({ mode: this.report.mode, case: r.case, group: r.group, status: r.status,
        stage_scores: stage, checks: r.checks, probes: r.probes, snapshots: r.snapshots }, null, 2),
      metadata: { sourceReport: this.config.report, suiteSHA256: this.report.suite_sha256,
        originalDurationMs: r.duration_ms, originalModelCalls: calls.length,
        originalModels: [...new Set(calls.map(c => c.model))],
        originalTokenUsage: { prompt, completion, total: prompt + completion }, stageScores: stage,
        note: 'Imported result: zero new model calls. UI latency is import time, not agent runtime.' }
    };
  }
}
module.exports = MemoryBenchProvider;
module.exports.validate = validate;
module.exports.stageScores = stageScores;
